// Command tape-drive is a small BYOS (build your own server) implementation for
// TRMNL e-ink devices. It serves the firmware API from a directory of Go HTML
// templates populated by JSON files on disk, screenshots each page with headless
// Chrome and hands the device a 1-bit PNG.
//
// Nothing about the payload is compiled in: templates and JSON are read from
// disk on every render, the JSON is passed to the template untouched, and the
// playlist is a manifest of template/data pairs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// version is set at build time: -X main.version=...
var version = "dev"

// Server holds the pages, the playlist cursor and the last device reports.
type Server struct {
	renderers []*Renderer
	pages     []Page
	baseURL   string
	apiKey    string
	interval  int
	started   time.Time

	mu       sync.Mutex
	devices  map[string]DeviceState
	cursor   int
	lastPage int
}

// DeviceState is what the firmware told us on its last display request.
type DeviceState struct {
	MAC            string
	Model          string
	Firmware       string
	BatteryVoltage string
	RSSI           string
	WakeReason     string
	LastSeen       time.Time
	Requests       int
}

func main() {
	addr := flag.String("addr", "0.0.0.0:2300", "listen address")
	baseURL := flag.String("base-url", "", "public URL the device should fetch images from (default: derived from -addr)")
	apiKey := flag.String("api-key", "", "require this Access-Token from devices (empty = open LAN mode)")
	interval := flag.Int("interval", 900, "seconds between page changes, reported to the device as refresh_rate")
	templatesDir := flag.String("templates", "templates", "directory holding the page templates")
	dataDir := flag.String("data", "data", "directory holding the JSON data files")
	pagesFile := flag.String("pages", "pages.json", "playlist manifest listing template/data pairs")
	chrome := flag.String("chrome", "", "Chrome/Chromium binary (default: auto-detect)")
	width := flag.Int("width", 800, "screen width in pixels")
	height := flag.Int("height", 480, "screen height in pixels")
	renderEvery := flag.Int("render-every", 0, "re-render at least every N seconds even when template and data are unchanged (for clocks); 0 = only on change")
	previewPath := flag.String("preview", "", "render every page to this PNG path and exit, without serving")
	renderPath := flag.String("render", "", "alias for -preview")
	scriptPath := flag.String("script", "", "shell script to run periodically, e.g. one that refreshes the JSON files")
	scriptEvery := flag.Duration("script-interval", 5*time.Minute, "how often to run -script, e.g. 30s or 5m (independent of -interval)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("tape-drive " + version)
		return
	}

	pages, err := loadPages(*pagesFile, *templatesDir, *dataDir)
	if err != nil {
		log.Fatalf("pages: %v", err)
	}

	if *chrome == "" {
		*chrome = findChrome()
	}
	if *chrome == "" {
		log.Fatal("no Chrome binary found: pass -chrome")
	}
	if *interval <= 0 {
		log.Fatal("-interval must be a positive number of seconds")
	}
	if *renderEvery < 0 {
		log.Fatal("-render-every cannot be negative")
	}
	if *scriptEvery <= 0 {
		log.Fatal("-script-interval must be positive")
	}
	if *scriptPath != "" {
		if _, err := os.Stat(*scriptPath); err != nil {
			log.Fatalf("script: %v", err)
		}
	}
	tick := time.Duration(*renderEvery) * time.Second

	renderers := make([]*Renderer, len(pages))
	for index, page := range pages {
		renderers[index] = NewRenderer(page, index+1, len(pages), *width, *height, *chrome, tick)
	}

	// A preview export runs once and exits, refreshing the data first if a
	// script was given, so the PNG matches what the device would get.
	if output := firstNonEmpty(*previewPath, *renderPath); output != "" {
		ctx := context.Background()
		if *scriptPath != "" {
			runScript(ctx, *scriptPath)
		}
		renderAll(ctx, renderers, pageNames(pages), output)
		return
	}

	if *scriptPath != "" {
		log.Printf("running %s every %s (first run now)", *scriptPath, *scriptEvery)
		go runScriptLoop(context.Background(), *scriptPath, *scriptEvery)
	}

	server := &Server{
		renderers: renderers,
		pages:     pages,
		baseURL:   strings.TrimSuffix(*baseURL, "/"),
		apiKey:    *apiKey,
		interval:  *interval,
		started:   time.Now(),
		devices:   map[string]DeviceState{},
	}

	if server.baseURL == "" {
		server.baseURL = "http://localhost" + portSuffix(*addr)
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	names := make([]string, len(pages))
	for index, page := range pages {
		names[index] = fmt.Sprintf("%s (%s)", page.Name, page.Data)
	}

	log.Printf("tape-drive %s: serving %s on %s, %d page(s) every %ds: %s",
		version, server.baseURL, *addr, len(pages), *interval, strings.Join(names, ", "))

	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func pageNames(pages []Page) []string {
	names := make([]string, len(pages))
	for index, page := range pages {
		names[index] = page.Name
	}
	return names
}

// firstNonEmpty returns the first value that is set, so a flag and its alias
// can share one code path.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// renderAll writes one PNG per page and exits, for checking layouts without the
// server. A single page keeps the exact path; several pages get -<name>.
func renderAll(ctx context.Context, renderers []*Renderer, names []string, base string) {
	for index, renderer := range renderers {
		path := base
		if len(renderers) > 1 {
			extension := filepath.Ext(path)
			path = strings.TrimSuffix(path, extension) + "-" + names[index] + extension
		}

		render, err := renderer.Current(ctx)
		if err != nil {
			log.Fatalf("render %s: %v", names[index], err)
		}
		if err := os.WriteFile(path, render.PNG, 0o644); err != nil {
			log.Fatalf("write %s: %v", path, err)
		}

		log.Printf("wrote %s (%s, %d bytes)", path, render.Filename, len(render.PNG))
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /up", s.health)
	mux.HandleFunc("GET /api/setup", s.setup)
	mux.HandleFunc("GET /api/display", s.display)
	mux.HandleFunc("GET /api/display/current", s.displayCurrent)
	mux.HandleFunc("POST /api/log", s.logs)
	mux.HandleFunc("GET /screens/{filename}", s.screen)
	mux.HandleFunc("GET /{$}", s.preview)
	return mux
}

func (s *Server) health(writer http.ResponseWriter, request *http.Request) {
	writer.WriteHeader(http.StatusOK)
	fmt.Fprintln(writer, "ok")
}

type setupResponse struct {
	Status     int    `json:"status"`
	APIKey     string `json:"api_key"`
	FriendlyID string `json:"friendly_id"`
	ImageURL   string `json:"image_url"`
	Message    string `json:"message"`
}

type displayResponse struct {
	Status               int     `json:"status"`
	ImageURL             string  `json:"image_url"`
	Filename             string  `json:"filename"`
	ImageURLTimeout      int     `json:"image_url_timeout"`
	RefreshRate          int     `json:"refresh_rate"`
	ResetFirmware        bool    `json:"reset_firmware"`
	UpdateFirmware       bool    `json:"update_firmware"`
	FirmwareURL          *string `json:"firmware_url"`
	SpecialFunction      string  `json:"special_function"`
	MaximumCompatibility bool    `json:"maximum_compatibility"`
	TemperatureProfile   string  `json:"temperature_profile"`
	TouchbarMode         string  `json:"touchbar_mode"`
}

// setup answers the firmware's one-time provisioning call.
func (s *Server) setup(writer http.ResponseWriter, request *http.Request) {
	mac := request.Header.Get("ID")
	firmware := request.Header.Get("FW-Version")
	model := request.Header.Get("Model")

	log.Printf("setup mac=%q model=%q firmware=%q", mac, model, firmware)

	imageURL := ""
	if render, err := s.renderers[0].Current(request.Context()); err == nil {
		imageURL = s.imageURL(render.Filename)
	}

	writeJSON(writer, http.StatusOK, setupResponse{
		Status:     http.StatusOK,
		APIKey:     s.setupKey(mac),
		FriendlyID: friendlyID(mac),
		ImageURL:   imageURL,
		Message:    "Welcome to " + s.baseURL,
	})
}

// display is called on every wake and must answer with a screen to draw. The
// playlist advances one page per wake.
func (s *Server) display(writer http.ResponseWriter, request *http.Request) {
	s.serveDisplay(writer, request, s.advance())
}

// displayCurrent answers with the last page served, without advancing.
func (s *Server) displayCurrent(writer http.ResponseWriter, request *http.Request) {
	s.serveDisplay(writer, request, s.lastServed())
}

func (s *Server) serveDisplay(writer http.ResponseWriter, request *http.Request, index int) {
	s.remember(request)

	if !s.authorized(request) {
		// Never assert reset_firmware for an unknown token: the firmware wipes
		// its WiFi credentials and API key when it sees that. Answering "not
		// registered yet" makes it re-run setup instead.
		log.Printf("display: rejected Access-Token from mac=%q", request.Header.Get("ID"))
		writeJSON(writer, http.StatusAccepted, map[string]any{
			"status":  http.StatusAccepted,
			"message": "Device is not registered yet.",
		})
		return
	}

	render, err := s.renderers[index].Current(request.Context())
	if err != nil {
		log.Printf("display: render failed: %v", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": http.StatusInternalServerError,
			"error":  err.Error(),
		})
		return
	}

	writeJSON(writer, http.StatusOK, displayResponse{
		Status:               0,
		ImageURL:             s.imageURL(render.Filename),
		Filename:             render.Filename,
		RefreshRate:          s.interval,
		ResetFirmware:        false,
		UpdateFirmware:       false,
		FirmwareURL:          nil,
		SpecialFunction:      "none",
		TemperatureProfile:   "default",
		TouchbarMode:         "tap",
		ImageURLTimeout:      0,
		MaximumCompatibility: false,
	})
}

func (s *Server) logs(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Logs []json.RawMessage `json:"logs"`
	}

	if err := json.NewDecoder(request.Body).Decode(&payload); err == nil {
		for _, entry := range payload.Logs {
			log.Printf("device log (%s): %s", request.Header.Get("ID"), entry)
		}
	}

	writer.WriteHeader(http.StatusNoContent)
}

// screen serves the rendered PNG for the filename the device was handed. The
// filename identifies the page, so every page is checked (renders are cached).
func (s *Server) screen(writer http.ResponseWriter, request *http.Request) {
	filename := request.PathValue("filename")

	for _, renderer := range s.renderers {
		render, err := renderer.Current(request.Context())
		if err != nil || render.Filename != filename {
			continue
		}

		writer.Header().Set("Content-Type", "image/png")
		writer.Header().Set("Content-Length", fmt.Sprint(len(render.PNG)))
		writer.Header().Set("Cache-Control", "public, max-age=86400")
		writer.Write(render.PNG)
		return
	}

	http.NotFound(writer, request)
}

// preview is a human page: every screen in the playlist, the data behind each
// one and the last device reports.
func (s *Server) preview(writer http.ResponseWriter, request *http.Request) {
	var body strings.Builder

	for index, renderer := range s.renderers {
		page := s.pages[index]

		fmt.Fprintf(&body, "<h2>%d. %s</h2>", index+1, html.EscapeString(page.Name))

		render, err := renderer.Current(request.Context())
		if err != nil {
			fmt.Fprintf(&body, "<p class=err>render failed: %s</p>", html.EscapeString(err.Error()))
			fmt.Fprintf(&body, "<p class=meta>template %s · data %s</p>",
				html.EscapeString(page.Template), html.EscapeString(page.Data))
			continue
		}

		data, _ := json.MarshalIndent(render.Data, "", "  ")
		fmt.Fprintf(&body, "<p class=meta>%s · rendered %s · %d bytes · data %s</p>",
			html.EscapeString(render.Filename),
			render.RenderedAt.Format("15:04:05"),
			len(render.PNG),
			html.EscapeString(page.Data))
		fmt.Fprintf(&body, `<img src="/screens/%s" alt="screen">`, html.EscapeString(render.Filename))
		fmt.Fprintf(&body, "<pre>%s</pre>", html.EscapeString(string(data)))
	}

	fmt.Fprintf(&body, "<h2>Devices</h2>%s", s.deviceTable())
	fmt.Fprintf(&body, "<p class=meta>interval %ds · uptime %s</p>",
		s.interval, time.Since(s.started).Round(time.Second))

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	previewTemplate.Execute(writer, template.HTML(body.String()))
}

// advance moves the playlist cursor and returns the page to serve.
func (s *Server) advance() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.cursor % len(s.renderers)
	s.cursor++
	s.lastPage = index
	return index
}

func (s *Server) lastServed() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastPage
}

func (s *Server) authorized(request *http.Request) bool {
	if s.apiKey == "" {
		return true
	}
	return request.Header.Get("Access-Token") == s.apiKey
}

// setupKey is the key handed to a device during setup. With no configured key
// the server is open on the LAN: it still returns a stable per-device value so
// the firmware has something to store and send back.
func (s *Server) setupKey(mac string) string {
	if s.apiKey != "" {
		return s.apiKey
	}
	return "local-" + friendlyID(mac)
}

func (s *Server) imageURL(filename string) string {
	return s.baseURL + "/screens/" + filename
}

func (s *Server) remember(request *http.Request) {
	mac := request.Header.Get("ID")

	s.mu.Lock()
	defer s.mu.Unlock()

	state := s.devices[mac]
	state.MAC = mac
	state.Model = request.Header.Get("Model")
	state.Firmware = request.Header.Get("FW-Version")
	state.BatteryVoltage = request.Header.Get("Battery-Voltage")
	state.RSSI = request.Header.Get("RSSI")
	state.WakeReason = request.Header.Get("Wake-Reason")
	state.LastSeen = time.Now()
	state.Requests++
	s.devices[mac] = state
}

func (s *Server) deviceTable() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.devices) == 0 {
		return "<p class=meta>No device has called /api/display yet.</p>"
	}

	var table strings.Builder
	table.WriteString("<table><tr><th>MAC</th><th>Model</th><th>Firmware</th><th>Battery</th><th>RSSI</th><th>Wakes</th><th>Last seen</th></tr>")
	for _, state := range s.devices {
		fmt.Fprintf(&table, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td></tr>",
			html.EscapeString(state.MAC),
			html.EscapeString(state.Model),
			html.EscapeString(state.Firmware),
			html.EscapeString(state.BatteryVoltage),
			html.EscapeString(state.RSSI),
			state.Requests,
			state.LastSeen.Format("15:04:05"))
	}
	table.WriteString("</table>")

	return table.String()
}

func friendlyID(mac string) string {
	cleaned := strings.ToUpper(strings.ReplaceAll(mac, ":", ""))
	if len(cleaned) > 6 {
		return cleaned[len(cleaned)-6:]
	}
	return cleaned
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	writer.Write(body)
}

var previewTemplate = template.Must(template.New("preview").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>tape-drive</title>
<style>
  body { background: #f4f4f4; color: #111; font: 15px/1.5 Verdana, sans-serif; margin: 0; padding: 24px; }
  h1 { font-size: 20px; margin: 0 0 4px; }
  h2 { font-size: 15px; margin: 24px 0 8px; }
  img { border: 1px solid #bbb; width: 800px; max-width: 100%; }
  pre { background: #fff; border: 1px solid #ddd; padding: 12px; overflow: auto; font-size: 12px; }
  table { border-collapse: collapse; background: #fff; font-size: 13px; }
  th, td { border: 1px solid #ddd; padding: 6px 10px; text-align: left; }
  .meta { color: #555; font-size: 13px; }
  .err { color: #b00020; }
</style>
</head>
<body>
<h1>tape-drive</h1>
<p class="meta">templates + JSON on disk, rendered with headless Chrome</p>
{{.}}
</body>
</html>
`))
