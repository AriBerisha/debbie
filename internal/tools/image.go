package tools

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif" // the formats image.Decode reads
	"image/jpeg"
	_ "image/png"
	"io"
	"os"

	"debbie/internal/provider"
)

// What read_file sends of an image. Models that see images take JPEG, PNG,
// GIF and WebP. Claude's API takes up to 10 MB per image, 5 MB through some
// clouds, both counted in base64, and refuses any dimension over 3,000 pixels
// once a request holds more than 20 images, which a long session of
// screenshots reaches; its newest models use at most 2,576 pixels of the long
// edge anyway.
//
// An image the API refuses is worse than a failed call: every later request
// repeats it, so the whole conversation fails from then on. So what is sent
// is checked first, all of it where the standard library can read the format.
const (
	maxImageFile   = 64 << 20  // the largest image file read at all
	maxImagePixels = 100 << 20 // the most pixels decoded, to bound the memory
	maxImageBytes  = 3_500_000 // what goes as it is; bigger ones are re-encoded
	maxImageEdge   = 2576      // the longest edge sent
	imageQuality   = 90        // of a re-encoded image
)

// imageType names the format of an image from the start of its file, ""
// for anything else.
func imageType(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// fileImageType is imageType for a file, "" when it can't be opened: reading
// it as text then says why.
func fileImageType(abs string) string {
	f, err := os.Open(abs)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 12)
	n, _ := io.ReadFull(f, head)
	return imageType(head[:n])
}

// readImage reads a file that is an image, for read_file: ok is false for
// any other, which is read as text. Whether the model may have it is for the
// session to say, since the tool serves models that can see and models that
// can't.
func readImage(abs, shown string) (res Result, ok bool, err error) {
	media := fileImageType(abs)
	if media == "" {
		return Result{}, false, nil
	}
	f, err := os.Open(abs)
	if err != nil {
		return Result{}, true, fmt.Errorf("%s: %w", shown, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImageFile+1))
	if err != nil {
		return Result{}, true, fmt.Errorf("%s: %w", shown, err)
	}
	if len(data) > maxImageFile {
		return Result{}, true, fmt.Errorf("%s is too large an image (over %s)", shown, sizeText(maxImageFile))
	}
	sent, sentMedia, w, h, sw, sh, err := imageToSend(data, media)
	if err != nil {
		return Result{}, true, fmt.Errorf("%s: %w", shown, err)
	}
	out := fmt.Sprintf("%s, an image of %d×%d pixels and %s, comes with this result.", shown, w, h, sizeText(int64(len(data))))
	if sw != w || sh != h {
		out += fmt.Sprintf(" It was scaled down to %d×%d pixels to send it.", sw, sh)
	}
	return Result{Output: out, Display: fmt.Sprintf("%d×%d", w, h), Images: []provider.Image{{MediaType: sentMedia, Data: sent}}}, true, nil
}

// imageToSend returns an image as it goes to the model, its format, and its
// size in pixels as it is and as it goes. One within the limits goes as it is.
func imageToSend(data []byte, media string) (out []byte, outMedia string, w, h, sw, sh int, err error) {
	if media == "image/webp" {
		w, h, err = checkWebP(data)
		return data, media, w, h, w, h, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, 0, 0, fmt.Errorf("it looks like an image, but can't be read: %v", err)
	}
	w, h = cfg.Width, cfg.Height
	if w < 1 || h < 1 || w*h > maxImagePixels {
		return nil, "", 0, 0, 0, 0, fmt.Errorf("at %d×%d pixels it can't be sent", w, h)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, 0, 0, fmt.Errorf("it looks like an image, but can't be read: %v", err)
	}
	if len(data) <= maxImageBytes && max(w, h) <= maxImageEdge {
		return data, media, w, h, w, h, nil
	}
	sw, sh = w, h
	if longest := max(w, h); longest > maxImageEdge {
		sw, sh = max(1, w*maxImageEdge/longest), max(1, h*maxImageEdge/longest)
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, scaleImage(src, sw, sh), &jpeg.Options{Quality: imageQuality}); err != nil {
		return nil, "", 0, 0, 0, 0, err
	}
	return b.Bytes(), "image/jpeg", w, h, sw, sh, nil
}

// checkWebP says the size of a WebP image, or why it can't be sent. The
// standard library has no decoder for it, so it can't be scaled down, and
// only its headers can be checked: the container's length, against a cut-off
// file, and the image's size (RFC 9649).
func checkWebP(data []byte) (w, h int, err error) {
	bad := fmt.Errorf("it looks like a WebP image, but can't be read")
	if len(data) < 30 || int64(binary.LittleEndian.Uint32(data[4:]))+8 > int64(len(data)) {
		return 0, 0, bad
	}
	le24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	switch string(data[12:16]) {
	case "VP8X": // extended: the canvas's size, each less one
		w, h = le24(data[24:])+1, le24(data[27:])+1
	case "VP8 ": // lossy: after a frame tag and a start code
		if !bytes.Equal(data[23:26], []byte{0x9d, 0x01, 0x2a}) {
			return 0, 0, bad
		}
		w, h = int(binary.LittleEndian.Uint16(data[26:])&0x3fff), int(binary.LittleEndian.Uint16(data[28:])&0x3fff)
	case "VP8L": // lossless: after a signature, 14 bits each, less one
		if data[20] != 0x2f {
			return 0, 0, bad
		}
		bits := binary.LittleEndian.Uint32(data[21:])
		w, h = int(bits&0x3fff)+1, int(bits>>14&0x3fff)+1
	default:
		return 0, 0, bad
	}
	if w < 1 || h < 1 {
		return 0, 0, bad
	}
	if len(data) > maxImageBytes || max(w, h) > maxImageEdge {
		return 0, 0, fmt.Errorf("a WebP image of %d×%d pixels and %s can't be scaled down here; convert it to PNG or JPEG first", w, h, sizeText(int64(len(data))))
	}
	return w, h, nil
}

// scaleImage averages an image down to dw×dh pixels, over white wherever it
// is transparent, since a JPEG has no transparency.
func scaleImage(src image.Image, dw, dh int) *image.RGBA {
	type sum struct{ r, g, b, n uint64 }
	sums := make([]sum, dw*dh)
	bounds := src.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()
	for y := range sh {
		row := y * dh / sh * dw
		for x := range sw {
			r, g, b, a := src.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA() // premultiplied
			s := &sums[row+x*dw/sw]
			over := uint64(0xffff - a)
			s.r += uint64(r) + over
			s.g += uint64(g) + over
			s.b += uint64(b) + over
			s.n++
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for i, s := range sums {
		if s.n == 0 {
			continue
		}
		p := out.Pix[i*4 : i*4+4 : i*4+4]
		p[0], p[1], p[2], p[3] = uint8(s.r/s.n>>8), uint8(s.g/s.n>>8), uint8(s.b/s.n>>8), 0xff
	}
	return out
}
