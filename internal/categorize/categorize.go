package categorize

import (
	"strings"
	"unicode"
)

// NormalizeMerchant produces a stable lookup key for merchant matching,
// preserving Greek and Latin letters while removing punctuation, Greek tonos, and common legal suffixes.
func NormalizeMerchant(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" {
		return ""
	}

	// Fold Greek accented vowels and final sigma so uppercase receipt text matches typed Greek.
	greekFold := strings.NewReplacer(
		"ά", "α", "έ", "ε", "ή", "η", "ί", "ι", "ό", "ο", "ύ", "υ", "ώ", "ω",
		"ϊ", "ι", "ϋ", "υ", "ΐ", "ι", "ΰ", "υ", "ς", "σ",
	)
	s = greekFold.Replace(s)
	s = strings.ReplaceAll(s, ".", "")

	// Replace remaining punctuation with spaces.
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}

	fields := strings.Fields(b.String())
	if len(fields) == 0 {
		return ""
	}

	// Drop trailing legal entity tokens.
	legal := map[string]bool{
		"αε": true, "αεε": true, "επε": true, "ικε": true, "οε": true, "εε": true,
		"ae": true, "sa": true, "ltd": true, "llc": true, "inc": true, "gmbh": true,
	}
	for len(fields) > 1 && legal[fields[len(fields)-1]] {
		fields = fields[:len(fields)-1]
	}

	return strings.Join(fields, " ")
}

type rule struct {
	category string
	keywords []string
}

var keywordRules = []rule{
	{
		category: "Groceries",
		keywords: []string{
			"σκλαβενιτησ", "σκλαβενιτη", "sklavenitis",
			"βασιλοπουλοσ", "βασιλοπουλο", "αβ βασιλοπουλος", "ab vassilopoulos",
			"μυκονοσ μαρκετ", "μασουτησ", "masoutis", "γαλαξιασ", "galaxias",
			"κριτικος", "κρητικοσ", "kritikos", "lidl", "my market", "mymarket",
			"market in", "bazaar", "supermarket", "σουπερ μαρκετ", "παντοπωλειο",
			"αρτοποιειο", "φουρνοσ", "bakery", "grocery", "groceries", "μαναβικο",
		},
	},
	{
		category: "Fuel",
		keywords: []string{
			"shell", "bp ", "eko", "εκο", "avin", "αβιν", "aegean", "revoil",
			"elin", "ελιν", "jet oil", "πρατηριο", "καυσιμα", "βενζινη", "petrol", "fuel", "gas station",
		},
	},
	{
		category: "Transport",
		keywords: []string{
			"οασα", "oasa", "μετρο", "metro", "αττικη οδοσ", "attiki odos", "διοδια", "tolls",
			"taxi", "ταξι", "uber", "freenow", "beat", "aegean air", "ryanair", "sky express",
			"blue star", "ferry", "ktel", "κτελ", "parking", "παρκινγκ", "trainose", "hellenic train",
		},
	},
	{
		category: "Utilities",
		keywords: []string{
			"δεη", "dei", "protergia", "heron", "ηρων", "elpedison", "zenith", "nrg",
			"ευδαπ", "eydap", "cosmote", "vodafone", "nova", "wind", "internet",
			"ρεύμα", "ρευμα", "ηλεκτρικ", "υδρευση", "φυσικο αεριο", "τηλεφων",
		},
	},
	{
		category: "Health",
		keywords: []string{
			"φαρμακειο", "pharmacy", "ιατρειο", "διαγνωστικο", "νοσοκομειο", "κλινικη",
			"οδοντιατρ", "doctor", "clinic", "hospital", "medical", "οπτικα", "optics",
		},
	},
	{
		category: "Dining",
		keywords: []string{
			"καφε", "cafe", "coffee", "espresso", "gregorys", "γρηγορησ", "everest",
			"coffee island", "starbucks", "εστιατοριο", "ταβερνα", "ψητοπωλειο", "σουβλακι",
			"restaurant", "bistro", "pizza", "burger", "bar", "efood", "wolt", "box",
		},
	},
	{
		category: "Entertainment",
		keywords: []string{
			"cinema", "σινεμα", "village", "netflix", "spotify", "steam", "playstation",
			"xbox", "θεατρο", "theater", "μουσειο", "museum", "συναυλια", "concert", "ticket",
		},
	},
	{
		category: "Shopping",
		keywords: []string{
			"skroutz", "amazon", "ikea", "jumbo", "public", "πλαισιο", "plaisio",
			"kotsovolos", "κωτσοβολοσ", "zara", "h&m", "bershka", "cosmos sport",
			"praktiker", "leroy merlin", "βιβλιοπωλειο", "bookstore",
		},
	},
}

// SuggestFromKeywords returns a category name based on merchant and optional OCR text,
// or "Uncategorized" when evidence is weak.
func SuggestFromKeywords(merchant string, ocrText string) string {
	haystack := NormalizeMerchant(merchant) + " " + strings.ToLower(ocrText)
	if strings.TrimSpace(haystack) == "" {
		return "Uncategorized"
	}

	for _, r := range keywordRules {
		for _, kw := range r.keywords {
			if strings.Contains(haystack, kw) {
				return r.category
			}
		}
	}
	return "Uncategorized"
}
