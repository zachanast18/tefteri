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

	"spending-project/internal/imageproc"
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

// Scan uses the same job slot and timeout for the initial read and any retry.
func (s *Scanner) Scan(ctx context.Context, jpegBytes []byte, loc *time.Location) (ParsedReceipt, string, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return ParsedReceipt{}, "", ErrBusy
	}

	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	text, err := s.scanText(runCtx, jpegBytes)
	if err != nil {
		return ParsedReceipt{}, "", err
	}
	parsed := ParseReceipt(text, loc)
	if parsed.PaymentHint != "" || parsed.AmountCents == 0 || runCtx.Err() != nil {
		return parsed, text, nil
	}

	cash, card := paymentEvidence(splitCleanLines(text))
	if cash && card {
		return parsed, text, nil
	}
	wider, err := imageproc.WidenReceiptForOCR(jpegBytes)
	if err != nil {
		return parsed, text, nil
	}
	if retryText, err := s.scanText(runCtx, wider); err == nil {
		parsed.PaymentHint = extractPaymentHint(splitCleanLines(retryText))
	}
	return parsed, text, nil
}

func (s *Scanner) scanText(ctx context.Context, jpegBytes []byte) (string, error) {
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

	cmd := exec.CommandContext(ctx, s.bin, tmpPath, "stdout", "-l", s.langs, "--psm", "6")
	cmd.Env = append(os.Environ(), fmt.Sprintf("OMP_THREAD_LIMIT=%d", s.threads))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
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
	dateDMYRe     = regexp.MustCompile(`\b(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{2,4})\b`)
	dateYMDRe     = regexp.MustCompile(`\b(20\d{2})[/.\-](\d{1,2})[/.\-](\d{1,2})\b`)
	amountRe      = regexp.MustCompile(`[+-]?\d+(?:[.,]\d{3})*[.,]\s*\d{2}\b`)
	cashLabel     = regexp.MustCompile(`(?:^|[^\pL])(?:[ΜM][ΕE][ΤT][ΡP][ΗH][ΤT][ΑA][ΕE]?|CASH)(?:$|[^\pL])`)
	cardLabel     = regexp.MustCompile(`(?:^|[^\pL])(?:[ΚK][ΑA][ΡP][ΤT](?:[ΑA]|[ΕE][ΣS])|CARDS?|VISA|MASTERCARD|MAESTRO|EFTPOS)(?:$|[^\pL])`)
	labelVariants = strings.NewReplacer("Ά", "Α", "Έ", "Ε", "Ή", "Η", "Ί", "Ι", "Ό", "Ο", "Ύ", "Υ", "Ώ", "Ω")
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
		"ΑΠΟΔΕΙΞ", "ΛΙΑΝΙΚ", "RECEIPT", "INVOICE", "ΤΑΜΕΙΟ",
		"ΗΜΕΡΟΜΗΝΙΑ", "WWW.", "HTTP", "ΚΑΛΩΣ ΟΡΙΣΑΤΕ", "WELCOME",
		"ΦΩΤΟΤΥΠΙΚΟ ΚΕΝΤΡΟ", "ΨΗΦΙΑΚΕΣ ΕΚΤΥΠΩΣΕΙΣ", "RETAIL CUSTOMER",
	}

	limit := len(lines)
	if limit > 8 {
		limit = 8
	}
	for _, line := range lines[:limit] {
		line = strings.TrimLeftFunc(line, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		line = strings.TrimRight(line, " |;=~—_")
		label := normalizeLabel(line)
		if merchantMetadata.MatchString(label) || containsAny(label, skipTokens) ||
			strings.HasSuffix(line, ":") || strings.Contains(line, "%") ||
			looksMostlyNumeric(line) || amountRe.MatchString(line) ||
			(!merchantWord.MatchString(line) && !shortMerchant.MatchString(label)) ||
			strings.Contains(line, " - ") && strings.Contains(line, "=") {
			continue
		}
		return line, false
	}
	return "", true
}

var (
	merchantMetadata = regexp.MustCompile(`^(?:Α\.?Φ\.?Μ|AFM|VAT|TIN|Δ\.?Ο\.?Υ|ΤΗΛ|TEL|FAX|ΥΠΟΚ|ΕΔΡΑ|ΓΕΜΗ|ΣΕΙΡΑ|ΤΑΜΙΑΣ|DATE|TIME|ΩΡΑ)(?:$|[^\pL])`)
	merchantWord     = regexp.MustCompile(`\pL{4,}`)
	shortMerchant    = regexp.MustCompile(`^\p{Lu}{1,3}\s*[&+]\s*\p{Lu}{1,3}$`)
)

func normalizeLabel(s string) string {
	return labelVariants.Replace(strings.ToUpper(s))
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
	var best *amountCandidate
	var strongCandidates []amountCandidate

	for idx, line := range lines {
		label := normalizeLabel(line)
		if excludedTotal.MatchString(label) {
			continue
		}

		score := 1
		var totalLabel string
		switch {
		case payableTotal.MatchString(label):
			score = 100
			totalLabel = payableTotal.FindString(label)
		case receiptTotal.MatchString(label):
			score = 60
			totalLabel = receiptTotal.FindString(label)
		case cardLabel.MatchString(label):
			score = 40
		case cashLabel.MatchString(label):
			// Cash tender can include change.
			continue
		}

		cents := lastAmount(line)
		if cents == 0 && totalLabel != "" && !strings.ContainsFunc(strings.TrimPrefix(label, totalLabel), unicode.IsLetter) &&
			idx+1 < len(lines) && standaloneAmount.MatchString(normalizeLabel(lines[idx+1])) {
			cents = lastAmount(lines[idx+1])
		}
		if cents == 0 {
			continue
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
	for _, c := range strongCandidates {
		if c.score == best.score && c.cents != best.cents {
			needsCheck = true
			break
		}
	}

	return best.cents, money.FormatInput(best.cents), needsCheck
}

var (
	payableTotal     = regexp.MustCompile(`^(?:ΤΕΛΙΚΟ ΣΥΝΟΛΟ|ΠΛΗΡΩΤΕΟ|ΣΥΝΟΛΟ ΠΛΗΡΩΜΗΣ|GRAND TOTAL|TOTAL DUE|AMOUNT DUE|AMOUNT PAID|TOTAL TO PAY|BALANCE DUE)(?:$|[^\pL])`)
	receiptTotal     = regexp.MustCompile(`^(?:ΣΥΝΟΛΟ|(?:YOUR )?TOTAL)(?:$|[^\pL])`)
	excludedTotal    = regexp.MustCompile(`(?:^|[^\pL])(?:ΜΕΡΙΚΟ|SUB[ -]?TOTAL|Φ\.?Π\.?Α|VAT|TAX|ΚΑΘΑΡΗ|NET|ΡΕΣΤΑ|CHANGE|ΕΚΠΤΩΣΗ|DISCOUNT|ΕΔΩΣΕ|TENDERED|CASH GIVEN|ΕΙΣΠΡΑΞΗ|ΣΥΝΟΛΟ ΕΙΔΩΝ|TOTAL ITEMS)(?:$|[^\pL])`)
	standaloneAmount = regexp.MustCompile(`^(?:(?:EUR|ΕΥΡΩ|[€$£])\s*)?[+-]?\d+(?:[.,]\d{3})*[.,]\s*\d{2}\s*(?:EUR|ΕΥΡΩ|[€$£])?$`)
)

func lastAmount(line string) int64 {
	var cents int64
	for _, match := range amountRe.FindAllStringIndex(line, -1) {
		if strings.HasPrefix(strings.TrimSpace(line[match[1]:]), "%") {
			continue
		}
		value, err := money.ParsePositive(strings.Join(strings.Fields(line[match[0]:match[1]]), ""))
		if err == nil && value <= 100_000_000 {
			cents = value
		}
	}
	return cents
}

func extractPaymentHint(lines []string) string {
	cash, card := paymentEvidence(lines)
	if card && !cash {
		return "card"
	}
	if cash && !card {
		return "cash"
	}
	return ""
}

func paymentEvidence(lines []string) (cash, card bool) {
	for _, line := range lines {
		label := normalizeLabel(line)
		cash = cash || cashLabel.MatchString(label)
		card = card || cardLabel.MatchString(label)
	}
	return cash, card
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
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
