package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"spending-project/internal/money"
)

var (
	ErrBusy    = errors.New("scanner is busy with another receipt; retry in a moment or enter manually")
	ErrTimeout = errors.New("receipt scan timed out; photo is kept so you can enter details manually")
)

// Scanner runs local Tesseract OCR with single-job concurrency and a timeout.
type Scanner struct {
	bin     string
	langs   string
	timeout time.Duration
	threads int
	sem     chan struct{}
}

// NewScanner creates a Scanner with a 1-slot concurrency semaphore.
func NewScanner(bin string, langs string, timeout time.Duration, threads int) *Scanner {
	if threads <= 0 {
		threads = 1
	}
	return &Scanner{
		bin:     bin,
		langs:   langs,
		timeout: timeout,
		threads: threads,
		sem:     make(chan struct{}, 1),
	}
}

// Scan runs Tesseract on the provided JPEG bytes. Returns ErrBusy immediately if
// another scan is already running.
func (s *Scanner) Scan(ctx context.Context, jpegBytes []byte) (string, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return "", ErrBusy
	}

	tmp, err := os.CreateTemp("", "receipt-*.jpg")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(jpegBytes); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, s.bin, tmpPath, "stdout", "-l", s.langs, "--psm", "6")
	cmd.Env = append(os.Environ(), fmt.Sprintf("OMP_THREAD_LIMIT=%d", s.threads))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", ErrTimeout
		}
		return "", fmt.Errorf("tesseract failed: %w", err)
	}

	return stdout.String(), nil
}

// ParsedReceipt contains extracted candidate fields and review flags.
type ParsedReceipt struct {
	Merchant      string
	CheckMerchant bool
	Date          string // YYYY-MM-DD
	CheckDate     bool
	AmountCents   int64
	AmountText    string
	CheckAmount   bool
	PaymentHint   string // "cash", "card", or ""
}

var (
	dateDMYRe = regexp.MustCompile(`\b(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{2,4})\b`)
	dateYMDRe = regexp.MustCompile(`\b(20\d{2})[/.\-](\d{1,2})[/.\-](\d{1,2})\b`)
	amountRe  = regexp.MustCompile(`(\d{1,3}(?:[.,]\d{3})*[.,]\d{2}|\d+[.,]\d{2})`)
)

// ParseReceipt extracts merchant, purchase date, final payable amount, and payment hint
// from Greek and English receipt OCR text.
func ParseReceipt(text string, loc *time.Location) ParsedReceipt {
	lines := splitCleanLines(text)
	var out ParsedReceipt

	out.Merchant, out.CheckMerchant = extractMerchant(lines)
	out.Date, out.CheckDate = extractDate(lines, loc)
	out.AmountCents, out.AmountText, out.CheckAmount = extractAmount(lines)
	out.PaymentHint = extractPaymentHint(lines)

	return out
}

func splitCleanLines(text string) []string {
	raw := strings.Split(text, "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimSpace(strings.ReplaceAll(l, "\r", ""))
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func extractMerchant(lines []string) (string, bool) {
	skipTokens := []string{
		"αποδειξη", "απόδειξη", "λιανικ", "receipt", "invoice", "ταμειο", "ταμείο",
		"αφμ", "α.φ.μ", "afm", "vat", "tin", "δου", "δ.ο.υ", "τηλ", "tel", "fax",
		"ημερομηνια", "date", "ωρα", "time", "www.", "http", "καλως ορισατε", "welcome",
	}

	limit := len(lines)
	if limit > 8 {
		limit = 8
	}
	for i := 0; i < limit; i++ {
		line := lines[i]
		low := strings.ToLower(line)
		if len([]rune(line)) < 3 {
			continue
		}
		if !hasLetters(line) || looksMostlyNumeric(line) || amountRe.MatchString(line) {
			continue
		}
		skip := false
		for _, tok := range skipTokens {
			if strings.Contains(low, tok) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		return line, false
	}
	return "", true
}

func extractDate(lines []string, loc *time.Location) (string, bool) {
	var found []string
	seen := map[string]bool{}

	for _, line := range lines {
		for _, m := range dateDMYRe.FindAllStringSubmatch(line, -1) {
			d, _ := strconv.Atoi(m[1])
			mo, _ := strconv.Atoi(m[2])
			y, _ := strconv.Atoi(m[3])
			if y < 100 {
				y += 2000
			}
			if formatted, ok := validateDate(y, mo, d, loc); ok {
				if !seen[formatted] {
					seen[formatted] = true
					found = append(found, formatted)
				}
			}
		}
		for _, m := range dateYMDRe.FindAllStringSubmatch(line, -1) {
			y, _ := strconv.Atoi(m[1])
			mo, _ := strconv.Atoi(m[2])
			d, _ := strconv.Atoi(m[3])
			if formatted, ok := validateDate(y, mo, d, loc); ok {
				if !seen[formatted] {
					seen[formatted] = true
					found = append(found, formatted)
				}
			}
		}
	}

	if len(found) == 0 {
		return "", true
	}
	if len(found) > 1 {
		return found[0], true
	}
	return found[0], false
}

func validateDate(y, m, d int, loc *time.Location) (string, bool) {
	if y < 2000 || y > 2099 || m < 1 || m > 12 || d < 1 || d > 31 {
		return "", false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
	if t.Year() != y || int(t.Month()) != m || t.Day() != d {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

type amountCandidate struct {
	cents int64
	score int
	line  int
}

func extractAmount(lines []string) (int64, string, bool) {
	strongLabels := []string{
		"τελικο συνολο", "τελικό σύνολο", "πληρωτεο", "πληρωτέο",
		"συνολο πληρωμης", "σύνολο πληρωμής", "grand total", "total due",
		"amount due", "total to pay", "balance due",
	}
	totalLabels := []string{
		"συνολο", "σύνολο", "total", "ποσο", "ποσό", "αξια", "αξία",
	}
	excludeLabels := []string{
		"μερικο", "μερικό", "subtotal", "sub-total", "sub total",
		"φπα", "φ.π.α", "vat", "tax", "καθαρη", "καθαρή", "net",
		"ρεστα", "ρέστα", "change", "εκπτωση", "έκπτωση", "discount",
		"εδωσε", "έδωσε", "tendered", "cash given", "εισπραξη", "είσπραξη",
	}

	var best *amountCandidate
	var strongCandidates []amountCandidate

	for idx, line := range lines {
		low := strings.ToLower(line)
		matches := amountRe.FindAllString(line, -1)
		if len(matches) == 0 {
			continue
		}

		// Parse last amount on the line (receipts put totals on the right).
		lastRaw := matches[len(matches)-1]
		cents, err := money.ParsePositive(lastRaw)
		if err != nil || cents > 100_000_000 {
			continue
		}

		excluded := containsAny(low, excludeLabels)
		isStrong := containsAny(low, strongLabels)
		isTotal := containsAny(low, totalLabels)

		// "ΜΕΤΡΗΤΑ 50,00" followed by "ΡΕΣΤΑ 4,20" is tendered cash, not payable total.
		if (strings.Contains(low, "μετρητα") || strings.Contains(low, "μετρητά") || strings.Contains(low, "cash")) &&
			!isStrong && !isTotal {
			excluded = true
		}

		if excluded {
			continue
		}

		score := 1
		if isStrong {
			score = 100
		} else if isTotal {
			score = 60
		} else if strings.Contains(low, "καρτα") || strings.Contains(low, "κάρτα") || strings.Contains(low, "card") || strings.Contains(low, "visa") || strings.Contains(low, "mastercard") {
			score = 40
		}

		cand := amountCandidate{cents: cents, score: score, line: idx}
		if score >= 60 {
			strongCandidates = append(strongCandidates, cand)
		}
		if best == nil || cand.score > best.score || (cand.score == best.score && cand.line > best.line) {
			c := cand
			best = &c
		}
	}

	if best == nil {
		return 0, "", true
	}

	// If we didn't match any explicit total label, flag for review.
	needsCheck := best.score < 60
	if len(strongCandidates) > 1 {
		// Check if strong candidates disagree on the amount.
		first := strongCandidates[0].cents
		for _, c := range strongCandidates[1:] {
			if c.cents != first && c.score == best.score {
				needsCheck = true
				break
			}
		}
	}

	return best.cents, money.FormatInput(best.cents), needsCheck
}

func extractPaymentHint(lines []string) string {
	for _, line := range lines {
		low := strings.ToLower(line)
		if strings.Contains(low, "καρτα") || strings.Contains(low, "κάρτα") ||
			strings.Contains(low, "visa") || strings.Contains(low, "mastercard") ||
			strings.Contains(low, "maestro") || strings.Contains(low, "eftpos") ||
			strings.Contains(low, "card") {
			return "card"
		}
		if strings.Contains(low, "μετρητα") || strings.Contains(low, "μετρητά") ||
			strings.Contains(low, "ρεστα") || strings.Contains(low, "ρέστα") ||
			strings.Contains(low, "cash") {
			return "cash"
		}
	}
	return ""
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func hasLetters(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func looksMostlyNumeric(s string) bool {
	digits := 0
	letters := 0
	for _, r := range s {
		if unicode.IsDigit(r) {
			digits++
		} else if unicode.IsLetter(r) {
			letters++
		}
	}
	return digits > letters*2
}
