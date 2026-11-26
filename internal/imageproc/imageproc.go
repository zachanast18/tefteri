package imageproc

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"strings"

	"golang.org/x/image/draw"
)

var (
	ErrUnsupportedType = errors.New("only JPEG and PNG images are supported")
	ErrTooLarge        = errors.New("image exceeds maximum allowed upload size")
	ErrDimsTooLarge    = errors.New("image dimensions exceed maximum allowed size")
)

// Processed holds the normalized JPEG bytes and metadata for storage and OCR.
type Processed struct {
	Data       []byte
	MimeType   string
	SHA256     string
	Width      int
	Height     int
	OrigWidth  int
	OrigHeight int
}

// Process validates, decodes, EXIF-orients, downscales if needed, and re-encodes
// a JPEG or PNG upload into a bounded JPEG suitable for OCR and SQLite storage.
func Process(r io.Reader, maxUploadBytes int64, maxInputDim int, maxStoredDim int) (*Processed, error) {
	limited := io.LimitReader(r, maxUploadBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if int64(len(raw)) > maxUploadBytes {
		return nil, ErrTooLarge
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrUnsupportedType
	}
	format = strings.ToLower(format)
	if format != "jpeg" && format != "png" {
		return nil, ErrUnsupportedType
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxInputDim || cfg.Height > maxInputDim {
		return nil, ErrDimsTooLarge
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	if format == "jpeg" {
		if orient := readExifOrientation(raw); orient > 1 {
			img = applyOrientation(img, orient)
		}
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w > maxStoredDim || h > maxStoredDim {
		nw, nh := fitBounds(w, h, maxStoredDim)
		dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
		img = dst
		w, h = nw, nh
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}

	encoded := out.Bytes()
	sum := sha256.Sum256(encoded)

	return &Processed{
		Data:       encoded,
		MimeType:   "image/jpeg",
		SHA256:     hex.EncodeToString(sum[:]),
		Width:      w,
		Height:     h,
		OrigWidth:  cfg.Width,
		OrigHeight: cfg.Height,
	}, nil
}

func fitBounds(w, h, maxDim int) (int, int) {
	if w >= h {
		nh := (h * maxDim) / w
		if nh < 1 {
			nh = 1
		}
		return maxDim, nh
	}
	nw := (w * maxDim) / h
	if nw < 1 {
		nw = 1
	}
	return nw, maxDim
}

// readExifOrientation parses JPEG APP1 EXIF Orientation (1..8). Returns 1 if absent.
func readExifOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	pos := 2
	for pos+4 <= len(data) {
		if data[pos] != 0xFF {
			break
		}
		marker := data[pos+1]
		if marker == 0xDA || marker == 0xD9 { // SOS or EOI
			break
		}
		if marker == 0x00 || (marker >= 0xD0 && marker <= 0xD7) {
			pos += 2
			continue
		}
		segLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if segLen < 2 || pos+2+segLen > len(data) {
			break
		}
		if marker == 0xE1 && segLen >= 8 && string(data[pos+4:pos+10]) == "Exif\x00\x00" {
			tiff := data[pos+10 : pos+2+segLen]
			return parseTiffOrientation(tiff)
		}
		pos += 2 + segLen
	}
	return 1
}

func parseTiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(tiff[2:4]) != 0x002A {
		return 1
	}
	ifdOffset := int(bo.Uint32(tiff[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(tiff) {
		return 1
	}
	count := int(bo.Uint16(tiff[ifdOffset : ifdOffset+2]))
	for i := 0; i < count; i++ {
		entry := ifdOffset + 2 + i*12
		if entry+12 > len(tiff) {
			break
		}
		tag := bo.Uint16(tiff[entry : entry+2])
		if tag == 0x0112 { // Orientation
			val := int(bo.Uint16(tiff[entry+8 : entry+10]))
			if val >= 1 && val <= 8 {
				return val
			}
			return 1
		}
	}
	return 1
}

func applyOrientation(src image.Image, orient int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	var dst *image.RGBA
	switch orient {
	case 2: // Flip horizontal
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(w-1-x, y, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
	case 3: // Rotate 180
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(w-1-x, h-1-y, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
	case 4: // Flip vertical
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(x, h-1-y, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
	case 5: // Transpose
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(y, x, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
	case 6: // Rotate 90 CW
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(h-1-y, x, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
	case 7: // Transverse
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(h-1-y, w-1-x, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
	case 8: // Rotate 270 CW (90 CCW)
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(y, w-1-x, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
	default:
		return src
	}
	return dst
}
