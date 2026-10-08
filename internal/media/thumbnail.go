package media

import (
	"errors"
	"image"
	"image/color"
	_ "image/jpeg" // register decoders for Thumbnail
	_ "image/png"
	"io"
)

// MaxThumbnailSourcePixels bounds what Thumbnail will decode (memory use is
// roughly 1.5-4 bytes per pixel).
const MaxThumbnailSourcePixels = 100_000_000

// ErrNotDecodable means the server cannot render this format (HEIC, video);
// the owner's browser renders those previews instead.
var ErrNotDecodable = errors.New("format cannot be decoded on the server")

// Thumbnail decodes a JPEG or PNG and returns it scaled so its longer side is
// at most maxSide, turned upright according to the EXIF orientation.
func Thumbnail(r io.ReadSeeker, maxSide, orientation int) (image.Image, error) {
	config, format, err := image.DecodeConfig(r)
	if err != nil || (format != "jpeg" && format != "png") {
		return nil, ErrNotDecodable
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width*config.Height > MaxThumbnailSourcePixels {
		return nil, ErrNotDecodable
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	source, _, err := image.Decode(r)
	if err != nil {
		return nil, err
	}
	return orient(downscale(source, maxSide), orientation), nil
}

// downscale averages every source pixel that falls into each target pixel.
func downscale(source image.Image, maxSide int) *image.RGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	scale := 1.0
	if longest := max(width, height); longest > maxSide {
		scale = float64(maxSide) / float64(longest)
	}
	targetWidth := max(1, int(float64(width)*scale+0.5))
	targetHeight := max(1, int(float64(height)*scale+0.5))
	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for ty := 0; ty < targetHeight; ty++ {
		y0 := bounds.Min.Y + ty*height/targetHeight
		y1 := max(y0+1, bounds.Min.Y+(ty+1)*height/targetHeight)
		for tx := 0; tx < targetWidth; tx++ {
			x0 := bounds.Min.X + tx*width/targetWidth
			x1 := max(x0+1, bounds.Min.X+(tx+1)*width/targetWidth)
			var red, green, blue, alpha, count uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, b, a := source.At(x, y).RGBA()
					red, green, blue, alpha = red+uint64(r), green+uint64(g), blue+uint64(b), alpha+uint64(a)
					count++
				}
			}
			target.SetRGBA(tx, ty, color.RGBA{
				R: uint8(red / count >> 8), G: uint8(green / count >> 8),
				B: uint8(blue / count >> 8), A: uint8(alpha / count >> 8),
			})
		}
	}
	return target
}

// orient applies an EXIF orientation (1-8) so the image displays upright.
func orient(source *image.RGBA, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return source
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	swap := orientation >= 5
	targetWidth, targetHeight := width, height
	if swap {
		targetWidth, targetHeight = height, width
	}
	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var tx, ty int
			switch orientation {
			case 2:
				tx, ty = width-1-x, y
			case 3:
				tx, ty = width-1-x, height-1-y
			case 4:
				tx, ty = x, height-1-y
			case 5:
				tx, ty = y, x
			case 6:
				tx, ty = height-1-y, x
			case 7:
				tx, ty = height-1-y, width-1-x
			case 8:
				tx, ty = y, width-1-x
			}
			target.SetRGBA(tx, ty, source.RGBAAt(x, y))
		}
	}
	return target
}
