package imageproc

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"

	"golang.org/x/image/draw"
)

// WidenReceiptForOCR gives thin thermal-print characters more room in a retry.
func WidenReceiptForOCR(data []byte) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	wider := image.NewGray(image.Rect(0, 0, bounds.Dx()*135/100, bounds.Dy()))
	draw.CatmullRom.Scale(wider, wider.Bounds(), img, bounds, draw.Src, nil)
	return encodeJPEG(wider, 95)
}

func receiptOCRImage(img image.Image) image.Image {
	img = resize(cropReceipt(img), 2300)
	bounds := img.Bounds()
	gray := image.NewGray(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	// Increase separation between dark text and the paper on thermal receipts.
	const black, white = 46, 184
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			luminance := int(color.GrayModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.Gray).Y)
			gray.Pix[y*gray.Stride+x] = uint8(min(255, max(0, (luminance-black)*255/(white-black))))
		}
	}
	return gray
}

// cropReceipt removes contrasting background and leaves ambiguous images intact.
func cropReceipt(img image.Image) image.Image {
	cropper, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	bounds := img.Bounds()
	if !ok || bounds.Dy() < bounds.Dx() || bounds.Dx() < 120 {
		return img
	}
	sample := resize(img, 320)
	b := sample.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 40 {
		return img
	}
	scores := make([]int, w)
	for y := h / 8; y < h*7/8; y++ {
		for x := range scores {
			scores[x] += int(color.GrayModel.Convert(sample.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y)
		}
	}
	for x := range scores {
		scores[x] /= h*7/8 - h/8
	}
	mean := func(start, end int) int {
		total := 0
		for _, score := range scores[start:end] {
			total += score
		}
		return total / (end - start)
	}
	paper := mean(w*2/5, w*3/5)
	findSides := func(border, edge int) (left, right int, ok bool) {
		leftBackground, rightBackground := mean(border, edge), mean(w-edge, w-border)
		if paper-leftBackground < 20 || paper-rightBackground < 20 {
			return 0, 0, false
		}
		left, right = w/2, w/2
		for left > edge && mean(left-3, left) > (paper+leftBackground)/2 {
			left--
		}
		for right < w-edge && mean(right, right+3) > (paper+rightBackground)/2 {
			right++
		}
		return left, right, left > edge && right < w-edge && right-left >= w/4
	}
	left, right, centered := findSides(w/20, w/6)
	if !centered {
		left, right, ok = findSides(w/40, w/12)
		if !ok {
			return img
		}
	}
	// Column averages follow the most common edge position. Keep a margin
	// for mild skew and curled edges.
	left = max(0, left-w/100)
	right = min(w, right+w/100)
	if !centered {
		// Remove a dark strip above off-center paper. It can be read as
		// several lines of text before the merchant header.
		var streak int
		midLeft, midRight := left+(right-left)/3, right-(right-left)/3
		for y := 0; y < h/4; y++ {
			var luminance int
			for x := midLeft; x < midRight; x++ {
				luminance += int(color.GrayModel.Convert(sample.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y)
			}
			if luminance >= (paper-16)*(midRight-midLeft) {
				streak++
				if streak == 5 {
					bounds.Min.Y += max(0, y-7) * bounds.Dy() / h
					break
				}
			} else {
				streak = 0
			}
		}
	}
	bounds.Max.X = bounds.Min.X + (right*bounds.Dx()+w-1)/w
	bounds.Min.X += left * img.Bounds().Dx() / w
	return cropper.SubImage(bounds)
}
