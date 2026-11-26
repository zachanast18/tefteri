package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"spending-project/internal/auth"
	"spending-project/internal/config"
	"spending-project/internal/crypto"
	"spending-project/internal/db"
	"spending-project/internal/ocr"
	"spending-project/internal/web"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	switch cmd {
	case "healthcheck":
		runHealthcheck(cfg)
	case "setup-account":
		runSetupAccount(cfg)
	case "backup":
		if len(os.Args) < 3 {
			log.Fatalf("usage: ledger backup <dest-file>")
		}
		runBackup(cfg, os.Args[2])
	case "rotate-kek":
		runRotateKEK(cfg)
	case "rotate-dek":
		if len(os.Args) < 3 {
			log.Fatalf("usage: ledger rotate-dek <household-id>")
		}
		hhID, err := strconv.ParseInt(os.Args[2], 10, 64)
		if err != nil || hhID <= 0 {
			log.Fatalf("invalid household id")
		}
		runRotateDEK(cfg, hhID)
	case "seed-demo":
		runSeedDemo(cfg)
	case "serve":
		runServe(cfg)
	default:
		log.Fatalf("unknown command: %s", cmd)
	}
}

func openConfiguredStore(cfg config.Config) *db.Store {
	kr, err := crypto.LoadKeyRing(cfg.MasterKeyFile, cfg.MasterKey)
	if err != nil {
		log.Fatalf("master key error: %v", err)
	}
	store, err := db.OpenWithKeys(cfg.DBPath, kr)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	return store
}

func runHealthcheck(cfg config.Config) {
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil || port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}

func runSetupAccount(cfg config.Config) {
	store := openConfiguredStore(cfg)
	defer store.Close()

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Household username [household]: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)
	if username == "" {
		username = "household"
	}

	fmt.Print("Password (hidden): ")
	p1, err := readPasswordLine(reader)
	fmt.Println()
	if err != nil {
		log.Fatalf("read password: %v", err)
	}
	if len(p1) < 8 {
		log.Fatalf("password must be at least 8 characters")
	}

	fmt.Print("Confirm password (hidden): ")
	p2, err := readPasswordLine(reader)
	fmt.Println()
	if err != nil {
		log.Fatalf("read password confirmation: %v", err)
	}
	if p1 != p2 {
		log.Fatalf("passwords do not match")
	}

	hash, err := auth.HashPassword(p1)
	if err != nil {
		log.Fatalf("hash password: %v", err)
	}
	if err := store.UpsertUser(context.Background(), username, hash); err != nil {
		log.Fatalf("save account: %v", err)
	}
	fmt.Printf("Household account %q saved.\n", username)

	if u, err := store.GetUserByUsername(context.Background(), username); err == nil {
		if n, _ := store.CountUnusedRecoveryCodes(context.Background(), u.ID); n == 0 {
			if plain, hashes, err := auth.GenerateRecoveryCodes(8); err == nil {
				if err := store.ReplaceRecoveryCodes(context.Background(), u.ID, hashes); err == nil {
					fmt.Println("Single-use recovery codes (save offline; shown once):")
					for _, c := range plain {
						fmt.Println(" ", c)
					}
				}
			}
		}
	}
}

func readPasswordLine(r *bufio.Reader) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		return string(b), err
	}
	// Fallback for non-TTY stdin (e.g. piped test input).
	s, err := r.ReadString('\n')
	return strings.TrimRight(s, "\r\n"), err
}

func runBackup(cfg config.Config, dest string) {
	store := openConfiguredStore(cfg)
	defer store.Close()

	if err := store.Backup(context.Background(), dest); err != nil {
		log.Fatalf("backup failed: %v", err)
	}
	fmt.Printf("Backup written to %s\n", dest)
}

func runRotateKEK(cfg config.Config) {
	store := openConfiguredStore(cfg)
	defer store.Close()

	if err := store.RotateKEK(context.Background()); err != nil {
		log.Fatalf("rotate-kek failed: %v", err)
	}
	fmt.Printf("All household DEKs and TOTP seeds rewrapped under KEK v%d\n", store.Keys.CurrentVersion)
}

func runRotateDEK(cfg config.Config, householdID int64) {
	store := openConfiguredStore(cfg)
	defer store.Close()

	if err := store.RotateHouseholdDEK(context.Background(), householdID); err != nil {
		log.Fatalf("rotate-dek failed: %v", err)
	}
	fmt.Printf("Rotated data encryption key and re-encrypted records for household %d\n", householdID)
}

func cleanStaleOCRTempFiles() {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "receipt-*.jpg"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

func runServe(cfg config.Config) {
	cleanStaleOCRTempFiles()
	store := openConfiguredStore(cfg)
	defer store.Close()

	scanner := ocr.NewScanner(cfg.TesseractBin, cfg.OCRLanguages, cfg.OCRTimeout, cfg.TesseractThread)
	srv, err := web.NewServer(cfg, store, scanner)
	if err != nil {
		log.Fatalf("init web server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Teftéri listening on %s (currency=%s, tz=%s, ocr=%s)", cfg.ListenAddr, cfg.Currency, cfg.Timezone, cfg.OCRLanguages)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func runSeedDemo(cfg config.Config) {
	store := openConfiguredStore(cfg)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().In(cfg.Location)
	ym := now.Format("2006-01")
	prevYM := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, cfg.Location).Format("2006-01")

	groc, _ := store.CategoryByName(ctx, 1, "Groceries")
	util, _ := store.CategoryByName(ctx, 1, "Utilities")
	fuel, _ := store.CategoryByName(ctx, 1, "Fuel")
	din, _ := store.CategoryByName(ctx, 1, "Dining")
	shop, _ := store.CategoryByName(ctx, 1, "Shopping")

	samples := []db.SaveExpenseInput{
		{Kind: "expense", AmountCents: 123456, Merchant: "ΠΡΟΜΗΘΕΥΤΙΚΗ ΕΛΛΑΔΟΣ ΑΝΩΝΥΜΗ ΕΜΠΟΡΙΚΗ ΕΤΑΙΡΕΙΑ ΤΡΟΦΙΜΩΝ", PurchaseDate: ym + "-03", PaymentMethod: "card", CategoryID: shop.ID, Note: "Synthetic demo: appliances", Source: "manual"},
		{Kind: "expense", AmountCents: 4580, Merchant: "ΣΚΛΑΒΕΝΙΤΗΣ Α.Ε.", PurchaseDate: ym + "-08", PaymentMethod: "cash", CategoryID: groc.ID, Note: "Synthetic demo", Source: "manual"},
		{Kind: "expense", AmountCents: 5420, Merchant: "SHELL ΚΗΦΙΣΙΑΣ", PurchaseDate: ym + "-12", PaymentMethod: "card", CategoryID: fuel.ID, Note: "Synthetic demo", Source: "manual"},
		{Kind: "refund", AmountCents: 1450, Merchant: "ΣΚΛΑΒΕΝΙΤΗΣ Α.Ε.", PurchaseDate: ym + "-14", PaymentMethod: "card", CategoryID: groc.ID, Note: "Synthetic demo: returned item", Source: "manual"},
		{Kind: "expense", AmountCents: 1890, Merchant: "ΓΡΗΓΟΡΗΣ ΜΙΚΡΟΓΕΥΜΑΤΑ", PurchaseDate: ym + "-18", PaymentMethod: "cash", CategoryID: din.ID, Note: "Synthetic demo", Source: "manual"},
		{Kind: "expense", AmountCents: 8940, Merchant: "ΔΕΗ Α.Ε.", PurchaseDate: prevYM + "-20", PaymentMethod: "card", CategoryID: util.ID, Note: "Synthetic demo", Source: "manual"},
	}

	for _, s := range samples {
		_, _ = store.SaveExpense(ctx, s)
	}

	_, _ = store.CreateSubscription(ctx, db.SaveSubscriptionInput{
		Name:             "Cosmote Fiber 200",
		AmountCents:      3290,
		Frequency:        "monthly",
		CategoryID:       util.ID,
		PaymentMethod:    "card",
		FirstRenewalDate: ym + "-01",
		NextDueDate:      ym + "-01",
		Status:           "active",
	}, cfg.Location)

	_, _ = store.CreateSubscription(ctx, db.SaveSubscriptionInput{
		Name:             "Home Insurance (Synthetic)",
		AmountCents:      24000,
		Frequency:        "yearly",
		CategoryID:       util.ID,
		PaymentMethod:    "card",
		FirstRenewalDate: ym + "-28",
		NextDueDate:      ym + "-28",
		Status:           "active",
	}, cfg.Location)

	fmt.Println("Synthetic demo data seeded.")
}
