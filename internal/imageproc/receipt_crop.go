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
	img = resize(cropReceiptSides(img), 2300)
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

// cropReceiptSides removes contrasting background to either side of a
// centered receipt. Keep the full height and leave ambiguous images intact.
func cropReceiptSides(img image.Image) image.Image {
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
	leftBackground, rightBackground := mean(w/20, w/6), mean(w*5/6, w*19/20)
	if paper-leftBackground < 20 || paper-rightBackground < 20 {
		return img
	}
	left, right := w/2, w/2
	for left > w/6 && mean(left-3, left) > (paper+leftBackground)/2 {
		left--
	}
	for right < w*5/6 && mean(right, right+3) > (paper+rightBackground)/2 {
		right++
	}
	if left == w/6 || right == w*5/6 || right-left < w/4 {
		return img
	}
	// Column averages follow the most common edge position. Keep a margin
	// for mild skew and curled edges.
	left = max(0, left-w/100)
	right = min(w, right+w/100)
	bounds.Max.X = bounds.Min.X + (right*bounds.Dx()+w-1)/w
	bounds.Min.X = img.Bounds().Min.X + left*img.Bounds().Dx()/w
	return cropper.SubImage(bounds)
}
