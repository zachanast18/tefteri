package imageproc

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

func TestProcessReceiptQR(t *testing.T) {
	const link = "https://example.gr/receipt?id=123&code=abc"
	bits, err := qrcode.NewQRCodeWriter().Encode(link, gozxing.BarcodeFormat_QR_CODE, 180, 180, nil)
	if err != nil {
		t.Fatal(err)
	}
	photo := image.NewGray(image.Rect(0, 0, 800, 1400))
	draw.Draw(photo, photo.Bounds(), image.White, image.Point{}, draw.Src)
	for y := 0; y < bits.GetHeight(); y++ {
		for x := 0; x < bits.GetWidth(); x++ {
			if bits.Get(x, y) {
				photo.SetGray(300+x, 1100+y, color.Gray{})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, photo); err != nil {
		t.Fatal(err)
	}
	got, err := ProcessReceipt(&buf, 1<<20, 2000, 500, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceiptURL != link {
		t.Fatalf("receipt URL = %q", got.ReceiptURL)
	}
	if got.Height != 500 || len(got.OCRData) == 0 {
		t.Fatalf("stored height = %d, OCR image bytes = %d", got.Height, len(got.OCRData))
	}
}

func TestReceiptURL(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)", "//example.gr/receipt", "https://user@example.gr",
		"https://example.gr\\@other.gr", "https://example.gr/\nreceipt",
	} {
		if got := receiptURL(raw); got != "" {
			t.Errorf("receiptURL(%q) = %q", raw, got)
		}
	}
}
