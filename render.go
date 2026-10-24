package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"html/template"
	"image"
	"image/png"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// PageInfo is what a template sees about the page it is rendering. The payload
// itself is available as .Data, decoded from JSON and otherwise untouched.
type PageInfo struct {
	Name  string
	Index int
	Total int
}

// Render is one rendered screen.
type Render struct {
	Key        string
	Filename   string
	PNG        []byte
	HTML       []byte
	RenderedAt time.Time
	Data       any
}

// Renderer turns one template plus one data file into a 1-bit PNG. It caches
// the last render and only re-runs the browser when the inputs change, so a
// device waking every few minutes never pays for a screenshot twice. A non-zero
// tick forces a fresh render (and a fresh .Now) every tick, for templates that
// show a clock.
type Renderer struct {
	page   Page
	info   PageInfo
	chrome string
	width  int
	height int
	tick   time.Duration

	mu      sync.Mutex
	current *Render
}

// NewRenderer wires one page of the manifest to a renderer.
func NewRenderer(page Page, index, total, width, height int, chrome string, tick time.Duration) *Renderer {
	return &Renderer{
		page:   page,
		info:   PageInfo{Name: page.Name, Index: index, Total: total},
		chrome: chrome,
		width:  width,
		height: height,
		tick:   tick,
	}
}

// Current returns the render matching the current template and data files.
func (r *Renderer) Current(ctx context.Context) (*Render, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tmplSource, err := os.ReadFile(r.page.Template)
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}

	dataSource := []byte("{}")
	switch raw, err := os.ReadFile(r.page.Data); {
	case err == nil:
		dataSource = raw
	case os.IsNotExist(err):
		// A fresh checkout has no data yet: render the empty state instead of
		// failing the device's wake.
		log.Printf("data file %s is missing, rendering an empty page", r.page.Data)
	default:
		return nil, fmt.Errorf("read data: %w", err)
	}

	key := inputKey(tmplSource, dataSource, r.width, r.height)

	// A clock in the template would otherwise freeze at the first render: the
	// cache only looks at the inputs. Bucketing the key by tick re-renders at
	// most once per tick.
	if r.tick > 0 {
		key = fmt.Sprintf("%s/%d", key, time.Now().Unix()/int64(r.tick/time.Second))
	}

	if r.current != nil && r.current.Key == key {
		return r.current, nil
	}

	render, err := r.render(ctx, key, tmplSource, dataSource)
	if err != nil {
		return nil, err
	}

	r.current = render
	return render, nil
}

func (r *Renderer) render(ctx context.Context, key string, tmplSource, dataSource []byte) (*Render, error) {
	var data any
	if err := json.Unmarshal(dataSource, &data); err != nil {
		return nil, fmt.Errorf("parse data %s: %w", r.page.Data, err)
	}

	tmpl, err := template.New(filepath.Base(r.page.Template)).
		Funcs(template.FuncMap{"plus": func(a, b int) int { return a + b }}).
		Parse(string(tmplSource))
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	view := struct {
		Page PageInfo
		Data any
		Now  time.Time
	}{Page: r.info, Data: normalize(data), Now: time.Now()}

	var page bytes.Buffer
	if err := tmpl.Execute(&page, view); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}

	shot, err := r.screenshot(ctx, page.Bytes())
	if err != nil {
		return nil, err
	}

	mono, err := toMono(shot, r.width, r.height)
	if err != nil {
		return nil, err
	}

	render := &Render{
		Key:        key,
		Filename:   filename(r.page.Name, key, time.Now()),
		PNG:        mono,
		HTML:       page.Bytes(),
		RenderedAt: time.Now(),
		Data:       data,
	}

	log.Printf("rendered %s (png %d bytes)", render.Filename, len(render.PNG))
	return render, nil
}

// normalize makes JSON numbers usable in templates: whole numbers become int64
// so `gt .Data.count 0` compares like an int, fractions stay float64.
func normalize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key := range typed {
			typed[key] = normalize(typed[key])
		}
		return typed
	case []any:
		for index := range typed {
			typed[index] = normalize(typed[index])
		}
		return typed
	case float64:
		if typed == math.Trunc(typed) && math.Abs(typed) < 1<<53 {
			return int64(typed)
		}
		return typed
	default:
		return value
	}
}

// screenshot loads the HTML into headless Chrome and captures the viewport.
func (r *Renderer) screenshot(ctx context.Context, page []byte) (image.Image, error) {
	if r.chrome == "" {
		return nil, fmt.Errorf("no Chrome binary found: pass -chrome")
	}

	dir, err := os.MkdirTemp("", "tape-drive-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	htmlPath := filepath.Join(dir, "page.html")
	shotPath := filepath.Join(dir, "shot.png")

	if err := os.WriteFile(htmlPath, page, 0o600); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-sync",
		"--force-device-scale-factor=1",
		"--default-background-color=ffffffff",
		fmt.Sprintf("--window-size=%d,%d", r.width, r.height),
		"--virtual-time-budget=3000",
		"--user-data-dir=" + filepath.Join(dir, "profile"),
		"--screenshot=" + shotPath,
		"file://" + htmlPath,
	}

	command := exec.CommandContext(ctx, r.chrome, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("chrome: %w: %s", err, stderr.String())
	}

	file, err := os.Open(shotPath)
	if err != nil {
		return nil, fmt.Errorf("chrome produced no screenshot: %w", err)
	}
	defer file.Close()

	shot, err := png.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("decode screenshot: %w", err)
	}

	if bounds := shot.Bounds(); bounds.Dx() != r.width || bounds.Dy() != r.height {
		return nil, fmt.Errorf("screenshot is %dx%d, want %dx%d",
			bounds.Dx(), bounds.Dy(), r.width, r.height)
	}

	return shot, nil
}

func inputKey(tmpl, data []byte, width, height int) string {
	hash := sha256.New()
	hash.Write(tmpl)
	hash.Write([]byte{0})
	hash.Write(data)
	hash.Write([]byte(fmt.Sprintf("%dx%d", width, height)))
	return hex.EncodeToString(hash.Sum(nil))
}

// filename builds the device cache key: <name>-<hash>-<unix time>, 31 chars
// max. Same inputs keep the same hash, so only new bytes change the name.
func filename(name, key string, at time.Time) string {
	short := key[:6]
	stamp := at.Unix()

	candidate := fmt.Sprintf("%s-%s-%d", name, short, stamp)
	if len(candidate) <= 31 {
		return candidate
	}

	// Long page names collapse to their first 7 characters, matching the
	// firmware's own truncation so the device's cache key stays stable.
	if len(name) > 7 {
		name = name[:7]
	}
	return fmt.Sprintf("%s-%s-%d", name, short, stamp)
}

// toMono converts the screenshot to 1-bit grayscale with Floyd-Steinberg
// dithering and encodes it as a 1-bit PNG (color type 0), which is what e-ink
// firmware expects and roughly a quarter of the 8-bit size.
func toMono(src image.Image, width, height int) ([]byte, error) {
	values := make([][]float64, height)
	for y := range values {
		values[y] = make([]float64, width)
		for x := range values[y] {
			red, green, blue, _ := src.At(x, y).RGBA()
			values[y][x] = 0.299*float64(red>>8) + 0.587*float64(green>>8) + 0.114*float64(blue>>8)
		}
	}

	rowBytes := (width + 7) / 8
	packed := make([]byte, rowBytes*height)

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			old := values[y][x]
			white := old >= 128
			if white {
				packed[y*rowBytes+x/8] |= 1 << (7 - uint(x%8))
			}

			target := 0.0
			if white {
				target = 255
			}
			diffuse(values, x, y, old-target)
		}
	}

	return encode1BitPNG(packed, width, height)
}

// diffuse spreads the quantization error to neighbouring pixels (7/16 right,
// 3/16 below-left, 5/16 below, 1/16 below-right).
func diffuse(values [][]float64, x, y int, err float64) {
	height := len(values)
	width := len(values[0])

	add := func(dx, dy int, factor float64) {
		nx, ny := x+dx, y+dy
		if nx < 0 || nx >= width || ny < 0 || ny >= height {
			return
		}
		values[ny][nx] += err * factor
	}

	add(1, 0, 7.0/16)
	add(-1, 1, 3.0/16)
	add(0, 1, 5.0/16)
	add(1, 1, 1.0/16)
}

// encode1BitPNG writes a grayscale PNG at bit depth 1. Go's encoder only emits
// 8-bit grayscale, and the panel budget favours 1-bit, so the container is
// assembled here.
func encode1BitPNG(packed []byte, width, height int) ([]byte, error) {
	rowBytes := (width + 7) / 8

	var raw bytes.Buffer
	for y := 0; y < height; y++ {
		raw.WriteByte(0) // filter: none
		raw.Write(packed[y*rowBytes : (y+1)*rowBytes])
	}

	var compressed bytes.Buffer
	if err := zlibCompress(&compressed, raw.Bytes()); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(height))
	ihdr[8] = 1 // bit depth
	ihdr[9] = 0 // color type: grayscale
	writeChunk(&out, "IHDR", ihdr)
	writeChunk(&out, "IDAT", compressed.Bytes())
	writeChunk(&out, "IEND", nil)

	return out.Bytes(), nil
}

func writeChunk(out *bytes.Buffer, kind string, data []byte) {
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	out.Write(length)
	out.WriteString(kind)
	out.Write(data)

	crc := crc32.ChecksumIEEE(append([]byte(kind), data...))
	sum := make([]byte, 4)
	binary.BigEndian.PutUint32(sum, crc)
	out.Write(sum)
}

func zlibCompress(out *bytes.Buffer, data []byte) error {
	writer := zlib.NewWriter(out)
	if _, err := writer.Write(data); err != nil {
		return err
	}
	return writer.Close()
}
