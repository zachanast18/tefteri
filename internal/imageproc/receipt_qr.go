package imageproc

import (
	"image"
	"net/url"
	"strings"
	"unicode"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

func decodeReceiptQR(img image.Image) string {
	// Try the footer first so a header or a logo doesn't obscure the receipt QR.
	var views []image.Image
	if cropper, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		bounds := img.Bounds()
		footer := bounds
		footer.Min.Y += bounds.Dy() / 2
		views = append(views, cropper.SubImage(footer))
		center := footer
		center.Min.X += bounds.Dx() / 4
		center.Max.X -= bounds.Dx() / 4
		center.Min.Y += bounds.Dy() / 6
		views = append(views, cropper.SubImage(center))
	}
	views = append(views, img)
	reader := qrcode.NewQRCodeReader()
	hints := map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_TRY_HARDER: true}
	for _, view := range views {
		bitmap, err := gozxing.NewBinaryBitmapFromImage(resize(view, 1800))
		if err != nil {
			continue
		}
		result, err := reader.Decode(bitmap, hints)
		if err == nil {
			if link := receiptURL(result.GetText()); link != "" {
				return link
			}
		}
	}
	return ""
}

func receiptURL(raw string) string {
	if strings.ContainsRune(raw, '\\') || strings.ContainsFunc(raw, unicode.IsControl) {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	return u.String()
}
