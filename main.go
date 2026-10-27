package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // the VM may not ship zoneinfo
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/bubbletea"
	"github.com/muesli/termenv"
)

var discordWebhook = os.Getenv("DISCORD_WEBHOOK")

type Shape struct {
	x, y   float64
	vx, vy float64
	form   int
	color  lipgloss.Color
}

var shapeChars = []string{"#", "@", "*", "+"}

var pageColors = []lipgloss.Color{
	lipgloss.Color("#cc0000"), // about
	lipgloss.Color("#663399"), // projects (rebeccapurple)
	lipgloss.Color("#D4A017"), // experience (mustard)
	lipgloss.Color("#069494"), // misc
}

type tickMsg time.Time

type model struct {
	width       int
	height      int
	shapes      []Shape
	currentPage string
	startTime   time.Time
	renderer    *lipgloss.Renderer
	sess        *session

	ticks    int // 50 ms ticks since the current photo was shown
	photo    int // about: which photo
	projSel  int // projects: highlighted row
	projOpen bool
	expOff   int // experience: first visible line
	search   *search
}

var (
	lastNotified = make(map[string]time.Time)
	scannerIPs   = make(map[string]time.Time)
	notifyMu     sync.Mutex
)

func notifyVisitor(s ssh.Session) {
	// Local test runs have no webhook: nothing to post to
	if discordWebhook == "" {
		return
	}

	startTime := time.Now()
	ip := strings.Split(s.RemoteAddr().String(), ":")[0]

	// Wait for session to end to measure duration
	<-s.Context().Done()
	duration := time.Since(startTime)

	notifyMu.Lock()
	// Check if scanner IP is rate-limited (1 hour)
	if t, ok := scannerIPs[ip]; ok && time.Since(t) < time.Hour {
		notifyMu.Unlock()
		return
	}
	// Check if real user IP is rate-limited (1 minute)
	if duration >= 15*time.Second {
		if t, ok := lastNotified[ip]; ok && time.Since(t) < time.Minute {
			notifyMu.Unlock()
			return
		}
		lastNotified[ip] = time.Now()
	} else {
		scannerIPs[ip] = time.Now()
	}
	notifyMu.Unlock()

	// Get location and timezone from IP (with timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://ip-api.com/json/" + ip)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var geo struct {
		City     string `json:"city"`
		Country  string `json:"country"`
		Timezone string `json:"timezone"`
	}
	json.Unmarshal(body, &geo)

	location := "Unknown"
	if geo.City != "" && geo.Country != "" {
		location = fmt.Sprintf("%s, %s", geo.City, geo.Country)
	}

	// Format time in visitor's local timezone
	localTime := startTime
	if geo.Timezone != "" {
		if loc, err := time.LoadLocation(geo.Timezone); err == nil {
			localTime = startTime.In(loc)
		}
	}

	// Format duration
	durationStr := fmt.Sprintf("%.1fs", duration.Seconds())
	if duration >= time.Minute {
		durationStr = fmt.Sprintf("%dm %ds", int(duration.Minutes()), int(duration.Seconds())%60)
	}

	var msg string
	if duration < 15*time.Second {
		msg = fmt.Sprintf(`{"content": "🤖 **Scanner detected**\n**IP:** %s\n**Location:** %s\n**Connected:** %s\n**Time:** %s\n_Muted for 1 hour_"}`,
			ip, location, durationStr, localTime.Format("Jan 2 15:04:05 MST"))
	} else {
		msg = fmt.Sprintf(`{"content": "🖥️ **SSH Portfolio visitor!**\n**IP:** %s\n**Location:** %s\n**Connected:** %s\n**Time:** %s"}`,
			ip, location, durationStr, localTime.Format("Jan 2 15:04:05 MST"))
	}

	if resp, err := client.Post(discordWebhook, "application/json", bytes.NewBuffer([]byte(msg))); err != nil {
		log.Error("Discord webhook failed", "error", err)
	} else {
		resp.Body.Close()
	}
}

func initialModel(renderer *lipgloss.Renderer, sess *session) model {
	shapes := make([]Shape, 4)
	for i := 0; i < 4; i++ {
		shapes[i] = Shape{
			x:     float64(rand.Intn(60) + 10),
			y:     float64(rand.Intn(15) + 3),
			vx:    float64(rand.Intn(3)-1) + 0.5, // {-0.5, 0.5, 1.5} — never zero
			vy:    float64(rand.Intn(3)-1) + 0.5,
			form:  i % len(shapeChars),
			color: pageColors[i],
		}
	}
	return model{
		width:       80,
		height:      24,
		shapes:      shapes,
		currentPage: "",
		startTime:   time.Now(),
		renderer:    renderer,
		sess:        sess,
		search:      &search{},
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Millisecond*50, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) Init() tea.Cmd {
	return tickCmd()
}

// goTo switches page and keeps the shared misc fetcher's subscriber count right.
func (m *model) goTo(page string) {
	if page != "misc" {
		m.search.close()
	}
	m.sess.setMisc(page == "misc")
	if page != m.currentPage {
		m.ticks = 0
		m.projOpen = false
		m.expOff = 0
	}
	m.currentPage = page
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// While the search box is focused every key belongs to it
		if m.currentPage == "misc" && m.search.open {
			return m, m.search.key(msg)
		}
		switch msg.String() {
		case "q":
			return m, tea.Quit
		case "1":
			m.goTo("about")
		case "2":
			m.goTo("projects")
		case "3":
			m.goTo("experience")
		case "4":
			m.goTo("misc")
		case "b", "esc":
			if m.currentPage == "projects" && m.projOpen {
				m.projOpen = false
			} else {
				m.goTo("")
			}
		default:
			return m.pageKey(msg)
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case searchTickMsg, searchResultMsg, trackDetailMsg:
		return m, m.search.handle(msg)
	case tickMsg:
		// 10-minute session timeout
		if time.Since(m.startTime) > 10*time.Minute {
			return m, tea.Quit
		}
		m.ticks++

		for i := range m.shapes {
			m.shapes[i].x += m.shapes[i].vx
			m.shapes[i].y += m.shapes[i].vy

			morphed := false

			if m.shapes[i].x <= 0 {
				m.shapes[i].x = 0
				m.shapes[i].vx *= -1
				morphed = true
			} else if m.shapes[i].x >= float64(m.width-5) {
				m.shapes[i].x = float64(m.width - 5)
				m.shapes[i].vx *= -1
				morphed = true
			}

			if m.shapes[i].y < 2 {
				m.shapes[i].y = 2
				m.shapes[i].vy *= -1
				morphed = true
			} else if m.shapes[i].y >= float64(m.height-3) {
				m.shapes[i].y = float64(m.height - 3)
				m.shapes[i].vy *= -1
				morphed = true
			}

			if morphed {
				m.shapes[i].form = (m.shapes[i].form + 1) % len(shapeChars)
			}
		}
		return m, tickCmd()
	}
	return m, nil
}

// pageKey handles the keys that only mean something on the current page.
func (m model) pageKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.currentPage {
	case "about":
		switch k {
		case "left", "h":
			m.photo = (m.photo + len(photos) - 1) % len(photos)
			m.ticks = 0
		case "right", "l":
			m.photo = (m.photo + 1) % len(photos)
			m.ticks = 0
		}
	case "projects":
		if m.projOpen {
			return m, nil
		}
		switch k {
		case "up", "k":
			m.projSel = (m.projSel + len(projects) - 1) % len(projects)
		case "down", "j":
			m.projSel = (m.projSel + 1) % len(projects)
		case "enter":
			m.projOpen = true
		}
	case "experience":
		maxOff := len(experienceLines(m.boxWidth()-8)) - m.viewRows()
		switch k {
		case "up", "k":
			m.expOff--
		case "down", "j":
			m.expOff++
		}
		m.expOff = clamp(m.expOff, 0, max(0, maxOff))
	case "misc":
		if k == "/" {
			m.search.open = true
			m.search.mode = modeHeard
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.currentPage != "" {
		return m.renderPage()
	}
	return m.renderHome()
}

func (m model) renderHome() string {
	bgStyle := m.renderer.NewStyle().Foreground(lipgloss.Color("#3a3a3a"))

	lines := make([][]rune, m.height)
	bgText := "vedsite "
	for y := 0; y < m.height; y++ {
		line := make([]rune, m.width)
		for x := 0; x < m.width; x++ {
			charIdx := (x + y*3) % len(bgText)
			line[x] = rune(bgText[charIdx])
		}
		lines[y] = line
	}

	for _, shape := range m.shapes {
		sx := int(shape.x)
		sy := int(shape.y)
		char := shapeChars[shape.form]

		for dy := 0; dy < 3; dy++ {
			for dx := 0; dx < 5; dx++ {
				py := sy + dy
				px := sx + dx
				if py >= 0 && py < m.height && px >= 0 && px < m.width {
					lines[py][px] = rune(char[0])
				}
			}
		}
	}

	shapeStyles := make([]lipgloss.Style, len(m.shapes))
	for i, shape := range m.shapes {
		shapeStyles[i] = m.renderer.NewStyle().Foreground(shape.color).Bold(true)
	}

	// Style whole runs of one colour instead of every character: same picture,
	// a fraction of the escape codes over the wire. A cell takes the colour of
	// the first shape covering it (-1 is the background).
	var output strings.Builder
	for y, line := range lines {
		runStart, runColor := 0, -2
		flush := func(end int) {
			if end <= runStart {
				return
			}
			style := bgStyle
			if runColor >= 0 {
				style = shapeStyles[runColor]
			}
			output.WriteString(style.Render(string(line[runStart:end])))
		}
		for x := range line {
			color := -1
			for i, shape := range m.shapes {
				sx, sy := int(shape.x), int(shape.y)
				if x >= sx && x < sx+5 && y >= sy && y < sy+3 {
					color = i
					break
				}
			}
			if color != runColor {
				flush(x)
				runStart, runColor = x, color
			}
		}
		flush(len(line))
		output.WriteString("\n")
	}

	footerStyle := m.renderer.NewStyle().
		Foreground(lipgloss.Color("#ffffff")).
		Background(lipgloss.Color("#1a1a1a")).
		Padding(0, 1)

	footer := footerStyle.Render("[1] about  [2] projects  [3] experience  [4] misc  [q] quit")
	output.WriteString("\n" + footer)

	return output.String()
}

// ── pages ────────────────────────────────────────────────────────────────────

const (
	minWidth  = 80
	minHeight = 24
)

// boxWidth is the page frame's width: the whole terminal for about (the art
// uses it), at most 100 columns elsewhere so text stays readable.
func (m model) boxWidth() int {
	if m.currentPage == "about" {
		return m.width
	}
	return min(m.width, 100)
}

// viewRows is the number of content rows inside the frame.
func (m model) viewRows() int { return m.height - 3 }

func (m model) renderPage() string {
	cv := newCanvas(m.width, m.height)
	if m.width < minWidth || m.height < minHeight {
		m.renderTooSmall(cv)
		return cv.render(m.styles())
	}
	switch m.currentPage {
	case "about":
		m.renderAbout(cv)
	case "projects":
		if m.projOpen {
			m.renderProject(cv)
		} else {
			m.renderProjectList(cv)
		}
	case "experience":
		m.renderExperience(cv)
	case "misc":
		m.renderMisc(cv)
	}
	return cv.render(m.styles())
}

func (m model) renderTooSmall(cv *canvas) {
	msg := []string{
		fmt.Sprintf("this terminal is %d×%d", m.width, m.height),
		"",
		"make the window at least 80×24",
		"and this page redraws itself.",
	}
	y0 := max(0, (m.height-len(msg))/2-1)
	for i, l := range msg {
		st := stDim
		if i == 0 {
			st = stFgB
		}
		cv.put(max(0, (m.width-strWidth(l))/2), y0+i, l, st)
	}
	m.footer(cv, 0, []key{{"b", "home"}, {"q", "quit"}})
}

// frame draws the rounded page border with the title in its top edge and
// returns the frame's left x.
func (m model) frame(cv *canvas, color, title int, name string) int {
	w := m.boxWidth()
	x0 := (m.width - w) / 2
	h := m.height - 1
	cv.put(x0, 0, "╭"+strings.Repeat("─", w-2)+"╮", color)
	for y := 1; y < h-1; y++ {
		cv.put(x0, y, "│", color)
		cv.put(x0+w-1, y, "│", color)
	}
	cv.put(x0, h-1, "╰"+strings.Repeat("─", w-2)+"╯", color)
	cv.put(x0+3, 0, " "+name+" ", title)
	return x0
}

type key struct{ k, label string }

var pageKeys = []key{{"1", "about"}, {"2", "projects"}, {"3", "experience"}, {"4", "misc"}}

// otherPages lists the number keys for every page except the ones named.
func otherPages(skip ...string) []key {
	var out []key
	for _, p := range pageKeys {
		skipped := false
		for _, s := range skip {
			skipped = skipped || p.label == s
		}
		if !skipped {
			out = append(out, p)
		}
	}
	return out
}

// footer draws the key hints in the home footer's style. If they don't fit,
// "experience" shortens to "exp" first, then hints drop from the middle.
func (m model) footer(cv *canvas, x0 int, keys []key) {
	text := func(ks []key) string {
		parts := make([]string, len(ks))
		for i, k := range ks {
			parts[i] = "[" + k.k + "] " + k.label
		}
		return " " + strings.Join(parts, "  ") + " "
	}
	t := text(keys)
	if strWidth(t) > m.width-x0 {
		for i := range keys {
			if keys[i].label == "experience" {
				keys[i].label = "exp"
			}
		}
		t = text(keys)
	}
	cv.put(x0, m.height-1, t, stFooter)
}

// ── about ──

var bio = "i was born in mumbai, india and spent most of my childhood playing sports. " +
	"i got to travel a lot, for roller skating competitions or just vacations. " +
	"i moved to boston fall 2024 and have had a blast at northeastern. " +
	"i love films, music, tv, math, philosophy, and a lot more."

var aboutLinks = []string{
	"https://github.com/deshpande-ved",
	"https://linkedin.com/in/deshpande-ved",
	"https://letterboxd.com/yerasaki",
	"mailto:deshpande.ved1@northeastern.edu",
}

type photo struct{ art, caption string }

var photos = []photo{
	{"hk", "hong kong int'l roller skating champion, age 10"},
	{"sem", "first semester of college"},
	{"buzz", "buzz cut → 52 weeks, 52 photos"},
	{"bday", "latest birthday, with my people"},
}

// buzz cut timing: one week per 3 ticks (150 ms), then hold the last week
const (
	buzzTicksPerFrame = 3
	buzzHoldFrames    = 10
)

func (m model) renderAbout(cv *canvas) {
	x0 := m.frame(cv, stR, stRB, "about me")
	ph := photos[m.photo]
	ind := fmt.Sprintf(" ← %d/%d → ", m.photo+1, len(photos))
	cv.put(m.width-2-strWidth(ind), 0, ind, stR)

	// as big as the terminal allows: two caption rows under it, a 32-column text column beside it
	a := art[ph.art]
	cols, rows := a.fit(m.width-38, m.viewRows()-2)
	frame := 0
	if a.frames() > 1 {
		frame = min(m.ticks/buzzTicksPerFrame%(a.frames()+buzzHoldFrames), a.frames()-1)
	}
	for y, row := range a.rendered(m.renderer.ColorProfile(), cols, rows, frame) {
		cv.putRaw(x0+2, 1+y, cols, row)
	}
	caps := wrap(ph.caption, max(cols, 24))
	if ph.art == "buzz" {
		caps = []string{ph.caption, fmt.Sprintf("week %d/52 · jul 2023 → jul 2024", frame+1)}
	}
	for i, c := range caps[:min(2, len(caps))] {
		cv.put(x0+2, 1+rows+i, c, stCap)
	}

	tx := x0 + 2 + cols + 2
	tw := m.width - tx - 2
	y := 2
	for _, l := range wrap(bio, tw) {
		cv.put(tx, y, l, stFg)
		y++
	}
	y++
	for _, l := range aboutLinks {
		shown := l
		if strWidth(shown) > tw {
			shown = strings.TrimPrefix(strings.TrimPrefix(shown, "https://"), "mailto:")
		}
		cv.putLink(tx, y, truncate(shown, tw), stLink, l)
		y++
	}
	// big terminals have room for every caption
	if y+2+len(photos) <= m.height-2 && m.height > minHeight {
		y++
		cv.put(tx, y, "photos", stDim)
		y++
		for i, p := range photos {
			if i == m.photo {
				cv.put(tx, y, truncate("► "+p.caption, tw), stR)
			} else {
				cv.put(tx, y, truncate("  "+p.caption, tw), stDim)
			}
			y++
		}
	}
	m.footer(cv, 0, append(append([]key{{"←/→", "photos"}}, otherPages("about")...), key{"b", "home"}, key{"q", "quit"}))
}

// ── projects ──

type project struct {
	title, short, tag, body string
	links                   []string
}

var projects = []project{
	{"Patrick: AINU Chatathon", "sourced patent briefs for solo founders", "runner-up, misneach track · $300 · built in a day",
		"Built in a day with cruz-andr: describe an invention and get a sourced patent brief for a solo founder's first attorney meeting. It cites real Google Patents records and quoted passages, and never fabricates a record or gives a legal verdict. I built the frontend, the AI output contract, an offline layout harness and sandboxed LaTeX/PDF export.",
		[]string{"https://ainu-chatathon.vercel.app", "https://vedsite.com/projects/patrick"}},
	{"Pi 5 Homelab", "NAS, backups, self-hosted services", "raspberry pi 5 · nas · nightly backups · live monitoring",
		"One Pi 5 runs my NAS, this site's API, SquashHub and Home Assistant. A 4 TB SSD holds everything and rsnapshot backs it up to a 2 TB HDD every night (7 daily, 4 weekly, 6 monthly, hard-linked). Only the public sites go out through a Cloudflare Tunnel; everything else stays on my tailnet. A live Beszel panel watches it all, and piplay streams films from the NAS to mpv.",
		[]string{"https://vedsite.com/projects/homelab"}},
	{"Home Assistant Smart Home", "wall kiosk, NFC switches, presence", "self-hosted · no vendor cloud",
		"Self-hosted on the Pi. An NFC tag on the door toggles both lamps, the plugs run ESPHome with no vendor cloud, and Siri and Apple Home work through the HomeKit Bridge. The wall kiosk shows the homelab panel, a location map, a copy of an MBTA countdown sign, Spotify and the weather.",
		[]string{"https://vedsite.com/projects/homeassistant"}},
	{"MacBook CCTV", "my webcam as an on-demand HA camera", "launchd · ffmpeg · mjpeg",
		"Home Assistant can't start a Mac's camera, so a small launchd Python server does. ffmpeg encodes MJPEG, the latest frame stays in memory, and it fans out to any number of viewers. The whole thing came down to one flag: -pixel_format nv12. LAN-only, and it shows up in Apple Home too.",
		[]string{"https://vedsite.com/projects/cctv"}},
	{"Tablet Wall Mount", "parametric 3D-printed kiosk rails", "openscad · 3d printed · tape-only",
		"Parametric OpenSCAD rails held up with tape only; the tablet slides in from the top, with a notch for the camera and room for USB-C. The tablet itself is debloated over ADB (no Google, F-Droid apps only), and its photos copy to the NAS automatically.",
		[]string{"https://vedsite.com/projects/wallmount"}},
	{"stopeating.pizza", "receipt in, graded health report out", "not live yet",
		"A grocery receipt photo goes in; out comes an A to E Nutri-Score per item, a cart score out of 10, flags for anything the EU restricts, and FDA/USDA recall checks. OCR runs first and a vision model only steps in when it's unsure. The scoring engine is standalone TypeScript with ~120 tests, validated against Open Food Facts' own regression suite.",
		[]string{"https://vedsite.com/projects/stopeating"}},
	{"Local AI", "Gemma 4 on my iPhone; a Mac agent I shelved", "on-device · kept one, shelved one",
		"Gemma 4 E4B runs fully on my iPhone 15 Pro, and I kept that. On the Mac I built Discord → OpenClaw → Qwen 2.5 7B on MLX → SearXNG. It worked end to end, then I shelved it: 2-3 minute replies, a 7B capability floor and a 16 GB RAM ceiling.",
		[]string{"https://local-llms-3br.pages.dev", "https://vedsite.com/projects/localai"}},
	{"SSH Portfolio", "this website, in your terminal", "you're in it",
		"A Go binary running natively on an Oracle Cloud VM. Every connection gets its own Bubble Tea session, with its own bouncing shapes. The misc page pulls live data from api.vedsite.com through one shared cache, so a hundred visitors cost the API the same as one.",
		[]string{"https://github.com/deshpande-ved/ssh-portfolio", "https://vedsite.com/projects/ssh"}},
	{"NEU SquashHub", "attendance, scheduling, emails for the club", "lead dev since spring 2026",
		"Club Squash's full-stack app: role-based access, live 4-digit attendance codes, ladders, scheduling, and announcements that email the team automatically. React → Express → PostgreSQL (3NF), three containers on my Pi.",
		[]string{"https://neusquashhub.com", "https://vedsite.com/projects/squashhub"}},
	{"Premier League Analysis", "does media coverage predict goals?", "ds3000 · team of four",
		"About 60 matches across 5 teams: results scraped from ESPN with Selenium, plus Guardian API article counts in the window before each match. Linear and polynomial regression came out at R² ≈ -0.11, so no: media coverage doesn't predict goals.",
		[]string{"https://vedsite.com/projects/pl"}},
	{"Sanguine Strategy Card Game", "Queen's Blood-style duel, minimax AI", "cs3500",
		"A Queen's Blood-style duel on a 3×5 board with pawns, costs and influence grids. Strict Java MVC with a Swing view, listener and strategy patterns, and AI players up to minimax. Playable in the browser on the site.",
		[]string{"https://vedsite.com/projects/sanguine"}},
	{"Light-Em-All Logic Game", "rotate wires until everything's lit", "cs2510",
		"Rotate wires until every tile is powered. Kruskal's algorithm with union-find generates the board and BFS spreads the power, with radius and hex modes. Playable in the browser on the site.",
		[]string{"https://vedsite.com/projects/lightemall"}},
	{"Boston Carbon Neutrality Sim", "plant eelgrass, convert drivers, hit zero", "cs2500",
		"Five real Boston neighborhoods with census commuter data: plant eelgrass and convert drivers to get emissions to zero before 15 seconds run out. Racket ISL+ with big-bang.",
		[]string{"https://vedsite.com/projects/carbon_neutrality"}},
}

func (m model) renderProjectList(cv *canvas) {
	x0 := m.frame(cv, stP, stPB, "projects")
	w := m.boxWidth()
	n := fmt.Sprintf(" %d ", len(projects))
	cv.put(x0+w-3-strWidth(n), 0, n, stP)
	descX := x0 + 40
	descW := w - 43
	for i, p := range projects {
		y := 2 + i
		if i == m.projSel {
			cv.put(x0+2, y, padRight("► "+truncate(p.title, 34), 38), stSel)
			cv.put(descX, y, padRight(truncate(p.short, descW), descW), stSelSub)
		} else {
			cv.put(x0+2, y, "  "+truncate(p.title, 34), stFg)
			cv.put(descX, y, truncate(p.short, descW), stDim)
		}
	}
	y := 2 + len(projects) + 1
	cv.put(x0+2, y, strings.Repeat("─", w-4), stDim)
	y++
	p := projects[m.projSel]
	cv.put(x0+2, y, truncate(p.tag, w-4), stP)
	y++
	room := m.height - 2 - y
	lines := wrap(p.body, w-4)
	more := "  [enter] for more"
	for i := 0; i < min(room, len(lines)); i++ {
		l := lines[i]
		if i == room-1 && len(lines) > room {
			l = truncate(l, w-4-strWidth(more)) + more
		}
		cv.put(x0+2, y+i, l, stFg)
	}
	m.footer(cv, x0, append(append([]key{{"↑/↓", "select"}, {"enter", "open"}}, otherPages("projects")...), key{"b", "home"}, key{"q", "quit"}))
}

func (m model) renderProject(cv *canvas) {
	x0 := m.frame(cv, stP, stPB, "projects")
	w := m.boxWidth()
	p := projects[m.projSel]
	n := fmt.Sprintf(" %d/%d ", m.projSel+1, len(projects))
	cv.put(x0+w-3-strWidth(n), 0, n, stP)
	cv.put(x0+3, 2, p.title, stPB)
	cv.put(x0+3, 3, truncate(p.tag, w-6), stDim)
	y := 5
	for _, l := range wrap(p.body, w-6) {
		cv.put(x0+3, y, l, stFg)
		y++
	}
	y++
	for _, l := range p.links {
		cv.put(x0+3, y, "→ ", stDim)
		cv.putLink(x0+5, y, truncate(l, w-8), stLink, l)
		y++
	}
	// [2] would just do what [b] does here
	m.footer(cv, x0, append(append([]key{{"b", "projects"}}, otherPages("projects")...), key{"q", "quit"}))
}

// ── experience ──

type expItem struct{ title, date, org, body string }
type expSection struct {
	heading string
	items   []expItem
}

var experience = []expSection{
	{"work", []expItem{
		{"Platform, API & Data Engineering Intern", "Jul – Dec 2026", "Colère Nu · Boston, MA",
			"Building the member app for iOS and Android from scratch in Expo and TypeScript, and own the versioned REST API and data layer on Next.js and Supabase, with bearer-token auth and row-level security. Load-tested 100 concurrent requests held to 11 Postgres connections; audited all 47 migrations."},
		{"Software Development Intern", "Jun – Aug 2023", "TalentHome Solutions LLP · Mumbai, India",
			"Integrated React components into existing web apps, restructured a MongoDB inventory system for faster queries, and ran SEO audits that lifted search rankings."},
	}},
	{"leadership", []expItem{
		{"Vice President", "Jan 2026 – Present", "Northeastern Club Squash · on the team since spring 2025",
			"Run inter-club operations (match scheduling, travel, talking to clubs across the east coast circuit) and the team's digital side through NEU SquashHub. Competed at CSA Nationals in Philadelphia, Feb 2025."},
		{"Program Alumni & Co-Facilitator", "Sep 2024 – Present", "Khoury FIRST",
			"1:1 mentoring for first-gen CS students and workshops on resumes, interviews and campus resources. Ran Spring 2026 orientation workshops for 30+ incoming freshmen."},
	}},
	{"education", []expItem{
		{"B.S. Computer Science", "Expected May 2028", "Northeastern University · concentration in systems, minor in math",
			"First co-op: Colère Nu, Jul – Dec 2026."},
	}},
}

var skills = []struct{ label1, label2, list string }{
	{"languages", "", "Java, Python, C, Go, TypeScript, JavaScript, SQL, HTML/CSS, Assembly, LISP, LaTeX · Marathi, Hindi, Gujarati"},
	{"tools &", "frameworks", "React.js, React Native (Expo), Next.js, Node.js, Flask, Express.js, PostgreSQL, Supabase, Docker, Linux, Tailscale, Cloudflare, Git, RESTful APIs, Nginx"},
}

type expLine struct {
	kind        int // 0 text, 1 title+date, 2 skill
	text, right string
	st          int
	indent      int
}

func experienceLines(width int) []expLine {
	var out []expLine
	for _, sec := range experience {
		out = append(out, expLine{text: sec.heading, st: stGB})
		for _, it := range sec.items {
			out = append(out, expLine{kind: 1, text: "► " + it.title, right: it.date})
			out = append(out, expLine{text: it.org, st: stDim, indent: 2})
			for _, l := range wrap(it.body, width-2) {
				out = append(out, expLine{text: l, st: stFg, indent: 2})
			}
			out = append(out, expLine{})
		}
	}
	for _, s := range skills {
		for i, l := range wrap(s.list, width-12) {
			label := ""
			if i == 0 {
				label = s.label1
			} else if i == 1 {
				label = s.label2
			}
			out = append(out, expLine{kind: 2, text: l, right: label})
		}
	}
	return out
}

func (m model) renderExperience(cv *canvas) {
	x0 := m.frame(cv, stG, stGB, "experience")
	w := m.boxWidth()
	lines := experienceLines(w - 8)
	view := m.viewRows()
	off := clamp(m.expOff, 0, max(0, len(lines)-view))
	for i, l := range lines[off:min(len(lines), off+view)] {
		y := 1 + i
		switch l.kind {
		case 1:
			cv.put(x0+3, y, truncate(l.text, w-8-strWidth(l.right)-2), stG)
			cv.put(x0+w-3-strWidth(l.right), y, l.right, stDim)
		case 2:
			cv.put(x0+3, y, l.right, stG)
			cv.put(x0+15, y, l.text, stFg)
		default:
			cv.put(x0+3+l.indent, y, l.text, l.st)
		}
	}
	keys := []key{}
	if len(lines) > view {
		// scrollbar on the right edge, and the hint only when there's something to scroll
		th := max(1, view*view/len(lines))
		pos := off * (view - th) / max(1, len(lines)-view)
		for y := 0; y < view; y++ {
			if y >= pos && y < pos+th {
				cv.put(x0+w-2, 1+y, "┃", stG)
			} else {
				cv.put(x0+w-2, 1+y, "│", stDim)
			}
		}
		keys = append(keys, key{"↑/↓", "scroll"})
	}
	m.footer(cv, x0, append(append(keys, otherPages("experience")...), key{"b", "home"}, key{"q", "quit"}))
}

// ── misc ──

func (m model) renderMisc(cv *canvas) {
	x0 := m.frame(cv, stT, stTB, "misc")
	w := m.boxWidth()
	cw := (w - 10) / 2
	L, R := x0+3, x0+3+cw+4
	d := misc.snapshot()
	s := m.search

	box := func(x int, active bool, text string, placeholder bool) {
		st := stDim
		if active {
			st = stT
		}
		cv.put(x, 1, "┌"+strings.Repeat("─", cw-2)+"┐", st)
		cv.put(x, 2, "│"+strings.Repeat(" ", cw-2)+"│", st)
		cv.put(x, 3, "└"+strings.Repeat("─", cw-2)+"┘", st)
		if active {
			q := string(s.query)
			// keep the end of a long query in view
			for strWidth(q) > cw-6 {
				_, size := utf8.DecodeRuneInString(q)
				q = q[size:]
			}
			cv.put(x+2, 2, q+"▏", stT)
			if placeholder {
				cv.put(x+3, 2, truncate(text, cw-6), stDim)
			}
		} else {
			cv.put(x+2, 2, truncate(text, cw-4), stDim)
		}
	}
	heardOn := s.open && s.mode == modeHeard
	seenOn := s.open && s.mode == modeSeen
	if heardOn {
		box(L, true, "type a song or artist", len(s.query) == 0)
	} else {
		box(L, false, "have i heard it?  [/]", false)
	}
	if seenOn {
		box(R, true, "type a film", len(s.query) == 0)
	} else {
		box(R, false, "have i seen it?  [/ then tab]", false)
	}

	textSt := func(stale bool) int {
		if stale {
			return stStale
		}
		return stFg
	}

	// left column: music
	y := 5
	cv.put(L, y, "top artists this month", stTB)
	y++
	if d.artists == nil {
		cv.put(L, y, "loading…", stDim)
	}
	for i, a := range firstN(d.artists, 3) {
		plays := fmt.Sprintf("%d plays", a.Playcount)
		cv.put(L, y+i, truncate(fmt.Sprintf("%d %s", i+1, a.Name), cw-strWidth(plays)-1), textSt(d.artistsStale))
		cv.put(L+cw-strWidth(plays), y+i, plays, stDim)
	}
	y += 4

	np := d.np
	playing := np != nil && np.IsPlaying
	if playing {
		cv.put(L, y, "now playing", stTB)
	} else {
		cv.put(L, y, "last played", stTB)
	}
	if d.npStale {
		cv.put(L+14, y, " stale ", stWarn)
	}
	y++
	switch {
	case np == nil:
		cv.put(L, y, "loading…", stDim)
	default:
		cv.put(L, y, truncate(np.TrackName, cw), map[bool]int{false: stFgB, true: stStale}[d.npStale])
		cv.put(L, y+1, truncate(np.ArtistName, cw), stDim)
		if playing {
			// interpolate between fetches, like misc.html
			prog := min(np.ProgressMs+int(time.Since(d.npAt).Milliseconds()), np.DurationMs)
			bw := cw - 13
			done := 0
			if np.DurationMs > 0 {
				done = prog * bw / np.DurationMs
			}
			cv.put(L, y+2, strings.Repeat("━", done), stT)
			cv.put(L+done, y+2, strings.Repeat("─", bw-done), stDim)
			cv.put(L+bw+1, y+2, mmss(prog)+" / "+mmss(np.DurationMs), stDim)
		} else {
			cv.put(L, y+2, "❚❚ paused", stDim)
		}
	}
	y += 4

	var rows []track
	var note string
	if playing {
		cv.put(L, y, "current queue", stTB)
		rows = d.queue
		if d.queue != nil && len(d.queue) == 0 {
			note = "nothing queued"
		}
		if d.queueFailed && d.queue == nil {
			note = "queue unavailable"
		}
	} else {
		cv.put(L, y, "recently played", stTB)
		rows = d.recent
		if d.recent != nil && len(d.recent) == 0 {
			note = "nothing played recently"
		}
		if d.recentFailed && d.recent == nil {
			note = "recently played unavailable"
		}
	}
	y++
	if note != "" {
		cv.put(L, y, note, stDim)
	} else if rows == nil && np != nil {
		cv.put(L, y, "loading…", stDim)
	}
	nameW := cw * 3 / 5
	for i, t := range firstN(rows, min(queueSlots, m.height-3-y)) {
		cv.put(L, y+i, truncate(t.TrackName, nameW-1), textSt(d.npStale))
		cv.put(L+nameW, y+i, truncate(t.ArtistName, cw-nameW), stDim)
	}

	// right column: films
	y = 5
	cv.put(R, y, "top 4", stTB)
	y++
	if d.top4 == nil {
		cv.put(R, y, "loading…", stDim)
	}
	for i, f := range firstN(d.top4, 4) {
		cv.put(R, y+i, truncate(fmt.Sprintf("%s (%s)", f.Title, f.Year), cw-7), textSt(d.filmsStale))
		cv.put(R+cw-5, y+i, stars(f.Rating), stT)
	}
	y += 5
	cv.put(R, y, "recent films", stTB)
	y++
	if d.films == nil {
		cv.put(R, y, "loading…", stDim)
	}
	for i, f := range firstN(d.films, 4) {
		cv.put(R, y+i, truncate(f.Title, cw-14), textSt(d.filmsStale))
		cv.put(R+cw-13, y+i, stars(f.Rating), stT)
		cv.put(R+cw-6, y+i, shortDate(f.WatchedDate), stDim)
	}
	y += 5
	cv.put(R, y, "elsewhere", stTB)
	y++
	for i, l := range []string{"https://letterboxd.com/yerasaki", "https://last.fm/user/yerasaki", "https://serializd.com/user/yerasaki"} {
		cv.putLink(R, y+i, truncate(l, cw), stLink, l)
	}

	if s.open {
		x := L
		if s.mode == modeSeen {
			x = R
		}
		m.renderResults(cv, x-1, cw+2)
		m.footer(cv, x0, []key{{"tab", "heard it ↔ seen it"}, {"↑/↓", "results"}, {"enter", "details"}, {"esc", "close search"}})
		return
	}
	m.footer(cv, x0, append(append([]key{{"/", "search"}}, otherPages("misc")...), key{"b", "home"}, key{"q", "quit"}))
}

// renderResults draws the search dropdown under the focused box.
func (m model) renderResults(cv *canvas, x, bw int) {
	s := m.search
	var lines []resultLine
	if s.message != "" {
		for _, l := range wrap(s.message, bw-4) {
			lines = append(lines, resultLine{text: l, st: stDim})
		}
	} else {
		for i, r := range s.results {
			sel := i == s.sel
			st, rst := stFg, stDim
			if sel {
				st, rst = stSelT, stSelT
			}
			lines = append(lines, resultLine{text: r.left, right: r.right, st: st, rst: rst, sel: sel})
			if sel && s.detailOpen {
				for _, d := range s.detail {
					for _, l := range wrap(d.text, bw-6) {
						lines = append(lines, resultLine{text: l, st: stT, indent: 2, link: d.link})
					}
				}
			}
		}
	}
	if len(lines) == 0 {
		return
	}
	// keep the selection in view when the list is taller than the room below the box
	room := m.height - 3 - 4 - 2
	start := 0
	for i, l := range lines {
		if l.sel && i >= room {
			start = i - room + 1
		}
	}
	lines = lines[start:min(len(lines), start+room)]

	y0 := 4
	cv.put(x, y0, "╭"+strings.Repeat("─", bw-2)+"╮", stT)
	for i, l := range lines {
		y := y0 + 1 + i
		cv.put(x, y, "│"+strings.Repeat(" ", bw-2)+"│", stT)
		inner := bw - 4
		if l.right != "" {
			left := padRight(truncate(l.text, inner-strWidth(l.right)-1), inner-strWidth(l.right))
			cv.put(x+2, y, left, l.st)
			cv.put(x+2+inner-strWidth(l.right), y, l.right, l.rst)
		} else if l.link != "" {
			cv.putLink(x+2+l.indent, y, truncate(l.text, inner-l.indent), stLink, l.link)
		} else {
			cv.put(x+2+l.indent, y, truncate(l.text, inner-l.indent), l.st)
		}
	}
	cv.put(x, y0+1+len(lines), "╰"+strings.Repeat("─", bw-2)+"╯", stT)
}

type resultLine struct {
	text, right string
	st, rst     int
	indent      int
	sel         bool
	link        string
}

// ── shared misc data ─────────────────────────────────────────────────────────
//
// One fetcher for every visitor: it only runs while at least one session is
// on the misc page, and sessions read the latest copy. On an error the last
// good data stays, marked stale.

const (
	apiBase    = "https://api.vedsite.com/api"
	queueSlots = 6
)

var httpClient = &http.Client{Timeout: 3 * time.Second}

type nowPlaying struct {
	TrackName  string `json:"track_name"`
	ArtistName string `json:"artist_name"`
	IsPlaying  bool   `json:"is_playing"`
	ProgressMs int    `json:"progress_ms"`
	DurationMs int    `json:"duration_ms"`
	Error      any    `json:"error"`
}

type track struct {
	TrackName  string `json:"track_name"`
	ArtistName string `json:"artist_name"`
}

type topArtist struct {
	Name      string `json:"name"`
	Playcount int    `json:"playcount"`
}

type film struct {
	Title       string  `json:"title"`
	Year        string  `json:"year"`
	Rating      float64 `json:"rating"`
	WatchedDate string  `json:"watched_date"`
}

type miscSnapshot struct {
	np            *nowPlaying
	npAt          time.Time
	npStale       bool
	queue, recent []track
	queueFailed   bool
	recentFailed  bool
	artists       []topArtist
	artistsStale  bool
	top4, films   []film
	filmsStale    bool
}

type miscCache struct {
	mu   sync.RWMutex
	data miscSnapshot
	subs int
	wake chan struct{}
	once sync.Once

	// fetch bookkeeping, only touched by the fetcher goroutine
	npDue, queueDue, artistsDue, filmsDue time.Time
	recentFor                             string
	recentRetry                           time.Time
}

var misc = &miscCache{wake: make(chan struct{}, 1)}

func (c *miscCache) snapshot() miscSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data
}

func (c *miscCache) subscribe(on bool) {
	c.once.Do(func() { go c.loop() })
	c.mu.Lock()
	if on {
		c.subs++
	} else {
		c.subs--
	}
	first := on && c.subs == 1
	c.mu.Unlock()
	if first {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (c *miscCache) loop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-c.wake:
			// first visitor after a quiet spell: everything is due now
			c.npDue, c.queueDue, c.artistsDue, c.filmsDue = time.Time{}, time.Time{}, time.Time{}, time.Time{}
			c.recentFor = ""
		}
		c.mu.RLock()
		active := c.subs > 0
		c.mu.RUnlock()
		if active {
			c.refresh()
		}
	}
}

// refresh runs the three groups in parallel so a slow recent-tracks lookup
// never holds up the film or artist columns.
func (c *miscCache) refresh() {
	now := time.Now()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); c.refreshPlayback(now) }()
	go func() { defer wg.Done(); c.refreshArtists(now) }()
	go func() { defer wg.Done(); c.refreshFilms(now) }()
	wg.Wait()
}

func (c *miscCache) refreshPlayback(now time.Time) {
	if now.After(c.npDue) {
		c.npDue = now.Add(5 * time.Second)
		var np nowPlaying
		err := getJSON(context.Background(), "/spotify/now-playing", &np)
		c.mu.Lock()
		if err != nil || np.Error != nil {
			c.data.npStale = c.data.np != nil
		} else {
			if c.data.np != nil && c.data.np.IsPlaying != np.IsPlaying {
				c.queueDue = time.Time{}
			}
			c.data.np, c.data.npAt, c.data.npStale = &np, now, false
		}
		np2 := c.data.np
		c.mu.Unlock()

		// recent-tracks does a Spotify lookup per song: only fetch it when we
		// enter paused mode, the paused track changes, or the last try failed
		if np2 != nil && !np2.IsPlaying {
			key := np2.TrackName + "__" + np2.ArtistName
			if key != c.recentFor || (!c.recentRetry.IsZero() && now.After(c.recentRetry)) {
				c.fetchRecent(np2)
			}
		} else {
			c.recentFor = ""
		}
	}

	c.mu.RLock()
	playing := c.data.np != nil && c.data.np.IsPlaying
	c.mu.RUnlock()
	if playing && now.After(c.queueDue) {
		c.queueDue = now.Add(10 * time.Second)
		var q struct {
			Queue []track `json:"queue"`
		}
		err := getJSON(context.Background(), "/spotify/queue", &q)
		c.mu.Lock()
		if err != nil {
			c.data.queueFailed = true
		} else {
			c.data.queue, c.data.queueFailed = nonNil(q.Queue), false
		}
		c.mu.Unlock()
	}
}

func (c *miscCache) refreshArtists(now time.Time) {
	if now.After(c.artistsDue) {
		c.artistsDue = now.Add(5 * time.Minute)
		var a struct {
			Artists []topArtist `json:"artists"`
		}
		err := getJSON(context.Background(), "/lastfm/top-artists", &a)
		c.mu.Lock()
		if err != nil {
			c.data.artistsStale = c.data.artists != nil
		} else {
			c.data.artists, c.data.artistsStale = nonNil(a.Artists), false
		}
		c.mu.Unlock()
	}
}

func (c *miscCache) refreshFilms(now time.Time) {
	if now.After(c.filmsDue) {
		c.filmsDue = now.Add(10 * time.Minute)
		var top, recent struct {
			Films []film `json:"films"`
		}
		err1 := getJSON(context.Background(), "/letterboxd/top4", &top)
		err2 := getJSON(context.Background(), "/letterboxd/recent", &recent)
		c.mu.Lock()
		if err1 == nil {
			c.data.top4 = nonNil(top.Films)
		}
		if err2 == nil {
			c.data.films = nonNil(recent.Films)
		}
		c.data.filmsStale = (err1 != nil || err2 != nil) && (c.data.top4 != nil || c.data.films != nil)
		c.mu.Unlock()
	}
}

func (c *miscCache) fetchRecent(np *nowPlaying) {
	var r struct {
		Tracks []track `json:"tracks"`
	}
	err := getJSON(context.Background(), "/lastfm/recent-tracks", &r)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.data.recentFailed = true
		c.recentRetry = time.Now().Add(10 * time.Second)
		return
	}
	// dedupe by name+artist and skip the paused track, like misc.html
	seen := map[string]bool{np.TrackName + "__" + np.ArtistName: true}
	unique := []track{}
	for _, t := range r.Tracks {
		k := t.TrackName + "__" + t.ArtistName
		if !seen[k] {
			seen[k] = true
			unique = append(unique, t)
		}
	}
	c.data.recent, c.data.recentFailed = unique, false
	c.recentFor = np.TrackName + "__" + np.ArtistName
	c.recentRetry = time.Time{}
}

// session tracks whether this connection is counted as a misc-page subscriber.
type session struct {
	mu     sync.Mutex
	onMisc bool
}

func (s *session) setMisc(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onMisc != on {
		s.onMisc = on
		misc.subscribe(on)
	}
}

func getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ── search ───────────────────────────────────────────────────────────────────

const (
	modeHeard = iota
	modeSeen
	searchDebounce = 150 * time.Millisecond
	maxQuery       = 64
)

type result struct {
	left, right   string
	artist, track string   // songs: what /lastfm/track needs
	facts         []detail // films: shown on enter, no fetch needed
}

type detail struct{ text, link string }

type search struct {
	open       bool
	mode       int
	query      []rune
	seq        int
	results    []result
	message    string
	sel        int
	detailOpen bool
	detail     []detail
	cancel     context.CancelFunc
}

type searchTickMsg struct{ seq int }
type searchResultMsg struct {
	seq     int
	results []result
	message string
}
type trackDetailMsg struct {
	seq, sel int
	detail   []detail
}

func (s *search) close() {
	if s.cancel != nil {
		s.cancel()
	}
	*s = search{seq: s.seq + 1}
}

func (s *search) key(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		s.close()
		return nil
	case tea.KeyTab:
		s.mode = 1 - s.mode
		return s.changed()
	case tea.KeyUp:
		if s.sel > 0 {
			s.sel--
			s.detailOpen = false
		}
		return nil
	case tea.KeyDown:
		if s.sel < len(s.results)-1 {
			s.sel++
			s.detailOpen = false
		}
		return nil
	case tea.KeyEnter:
		return s.toggleDetail()
	case tea.KeyBackspace:
		if len(s.query) > 0 {
			s.query = s.query[:len(s.query)-1]
			return s.changed()
		}
		return nil
	case tea.KeyCtrlU:
		s.query = nil
		return s.changed()
	case tea.KeySpace:
		return s.typed([]rune{' '})
	case tea.KeyRunes:
		return s.typed(msg.Runes)
	}
	return nil
}

func (s *search) typed(r []rune) tea.Cmd {
	for _, c := range r {
		if len(s.query) < maxQuery && c >= ' ' {
			s.query = append(s.query, c)
		}
	}
	return s.changed()
}

// changed starts the 150 ms debounce; a newer keystroke bumps seq so older
// ticks and responses are dropped.
func (s *search) changed() tea.Cmd {
	s.seq++
	s.sel, s.detailOpen = 0, false
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if len([]rune(strings.TrimSpace(string(s.query)))) < 2 {
		s.results, s.message = nil, ""
		return nil
	}
	seq := s.seq
	return tea.Tick(searchDebounce, func(time.Time) tea.Msg { return searchTickMsg{seq} })
}

func (s *search) handle(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case searchTickMsg:
		if msg.seq != s.seq || !s.open {
			return nil
		}
		q, mode := strings.TrimSpace(string(s.query)), s.mode
		if r, ok := cachedSearch(mode, q); ok {
			s.results, s.message = r.results, r.message
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		return func() tea.Msg {
			r := runSearch(ctx, mode, q)
			r.seq = msg.seq
			return r
		}
	case searchResultMsg:
		if msg.seq == s.seq {
			s.results, s.message, s.sel = msg.results, msg.message, 0
		}
	case trackDetailMsg:
		if msg.seq == s.seq && msg.sel == s.sel && s.detailOpen {
			s.detail = msg.detail
		}
	}
	return nil
}

func (s *search) toggleDetail() tea.Cmd {
	if s.sel >= len(s.results) {
		return nil
	}
	if s.detailOpen {
		s.detailOpen = false
		return nil
	}
	s.detailOpen = true
	r := s.results[s.sel]
	if s.mode == modeSeen {
		s.detail = r.facts
		return nil
	}
	s.detail = []detail{{text: "loading…"}}
	seq, sel := s.seq, s.sel
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var t struct {
			Plays       int   `json:"plays"`
			FirstPlayed int64 `json:"first_played"`
			LastPlayed  int64 `json:"last_played"`
		}
		params := url.Values{"artist": {r.artist}, "track": {r.track}}
		if err := getJSON(ctx, "/lastfm/track?"+params.Encode(), &t); err != nil {
			return trackDetailMsg{seq, sel, []detail{{text: "dates unavailable"}}}
		}
		d := []detail{{text: "played " + times(t.Plays)}}
		if t.LastPlayed != 0 {
			lp := time.Unix(t.LastPlayed, 0)
			d = append(d, detail{text: "last " + longDate(lp) + " · " + timeAgo(lp)})
		}
		if t.FirstPlayed != 0 && t.FirstPlayed != t.LastPlayed {
			fp := time.Unix(t.FirstPlayed, 0)
			d = append(d, detail{text: "first " + longDate(fp) + " · " + timeAgo(fp)})
		}
		return trackDetailMsg{seq, sel, d}
	}
}

func runSearch(ctx context.Context, mode int, q string) searchResultMsg {
	path := "/lastfm/search?q=" + url.QueryEscape(q)
	if mode == modeSeen {
		path = "/letterboxd/search?q=" + url.QueryEscape(q)
	}
	var out searchResultMsg
	if mode == modeHeard {
		var r struct {
			Indexing bool `json:"indexing"`
			Songs    []struct {
				Artist string `json:"artist"`
				Track  string `json:"track"`
				Plays  int    `json:"plays"`
			} `json:"songs"`
		}
		err := getJSON(ctx, path, &r)
		switch {
		case r.Indexing:
			return searchResultMsg{message: "still indexing my library, try again in a minute"}
		case ctx.Err() != nil:
			return searchResultMsg{seq: -1}
		case err != nil:
			return searchResultMsg{message: "search unavailable"}
		case len(r.Songs) == 0:
			out.message = "not in my scrobbles"
		}
		for _, sg := range r.Songs {
			plural := "s"
			if sg.Plays == 1 {
				plural = ""
			}
			out.results = append(out.results, result{left: sg.Track + " — " + sg.Artist,
				right: fmt.Sprintf("%d play%s", sg.Plays, plural), artist: sg.Artist, track: sg.Track})
		}
	} else {
		var r struct {
			Films []struct {
				Title        string  `json:"title"`
				Year         string  `json:"year"`
				Rating       float64 `json:"rating"`
				URL          string  `json:"url"`
				TimesWatched int     `json:"times_watched"`
				Rewatches    int     `json:"rewatches"`
				FirstWatched string  `json:"first_watched"`
				LastWatched  string  `json:"last_watched"`
			} `json:"films"` // tags are personal: never read
		}
		err := getJSON(ctx, path, &r)
		switch {
		case ctx.Err() != nil:
			return searchResultMsg{seq: -1}
		case err != nil:
			return searchResultMsg{message: "search unavailable"}
		case len(r.Films) == 0:
			out.message = "not in my diary"
		}
		for _, f := range r.Films {
			var facts []detail
			if f.LastWatched != "" {
				// a first logged watch marked "rewatch" means the original viewing
				// was never logged, so it counts on top of the logged ones
				watched := "watched " + times(max(f.TimesWatched, f.Rewatches+1))
				if f.Rewatches > 0 {
					es := "es"
					if f.Rewatches == 1 {
						es = ""
					}
					watched += fmt.Sprintf(" · %d rewatch%s", f.Rewatches, es)
				}
				facts = append(facts, detail{text: watched})
				if lw, err := time.ParseInLocation("2006-01-02", f.LastWatched, home); err == nil {
					facts = append(facts, detail{text: "last " + longDate(lw) + " · " + timeAgo(lw)})
				}
				if f.FirstWatched != f.LastWatched {
					if fw, err := time.ParseInLocation("2006-01-02", f.FirstWatched, home); err == nil {
						facts = append(facts, detail{text: "first " + longDate(fw) + " · " + timeAgo(fw)})
					}
				}
			} else {
				facts = append(facts, detail{text: "watched · no date logged"})
			}
			if f.URL != "" {
				facts = append(facts, detail{text: f.URL, link: f.URL})
			}
			title := f.Title
			if f.Year != "" {
				title += " (" + f.Year + ")"
			}
			right := ""
			if f.Rating > 0 {
				right = stars(f.Rating)
			}
			out.results = append(out.results, result{left: title, right: right, facts: facts})
		}
	}
	storeSearch(mode, q, out)
	return out
}

// a small shared cache so repeated queries (and tab back and forth) don't hit the API
var (
	searchCacheMu sync.Mutex
	searchCache   = map[string]cachedResult{}
)

type cachedResult struct {
	searchResultMsg
	at time.Time
}

func cacheKey(mode int, q string) string { return strconv.Itoa(mode) + ":" + strings.ToLower(q) }

func cachedSearch(mode int, q string) (searchResultMsg, bool) {
	searchCacheMu.Lock()
	defer searchCacheMu.Unlock()
	r, ok := searchCache[cacheKey(mode, q)]
	if !ok || time.Since(r.at) > 5*time.Minute {
		return searchResultMsg{}, false
	}
	return r.searchResultMsg, true
}

func storeSearch(mode int, q string, r searchResultMsg) {
	searchCacheMu.Lock()
	defer searchCacheMu.Unlock()
	if len(searchCache) >= 256 {
		searchCache = map[string]cachedResult{}
	}
	searchCache[cacheKey(mode, q)] = cachedResult{r, time.Now()}
}

// ── art ──────────────────────────────────────────────────────────────────────

//go:embed art/*.json.gz
var artFS embed.FS

// artPiece is one photo (or the 52 buzz cut frames) at the biggest size the
// page ever draws it: one brightness and one colour value per terminal cell.
type artPiece struct {
	cols, rows int
	lum        [][]byte // [frame] brightness per cell, already contrast-stretched
	rgb        [][]byte // [frame] r,g,b per cell

	mu        sync.Mutex
	cache     map[artKey][]string // rendered rows, shared by every session
	order     []artKey            // oldest first, for eviction
	cacheSize int                 // bytes held in cache
}

type artKey struct {
	profile           termenv.Profile
	cols, rows, frame int
}

// artCacheBytes bounds each piece's cache; visitors' terminal sizes vary, and
// the buzz cut at one size is 52 frames.
const artCacheBytes = 8 << 20

// the brightness ramp from terminalcam, dark to bright
const ramp = " .'`^\",:;Il!i><~+_-?][}{1)(|/tfjrxnuvczXYUJCLQ0OZmwqpdbkhao*#MW&8%B@$"

var art = map[string]*artPiece{}

func (a *artPiece) frames() int { return len(a.lum) }

// fit is the largest size within maxCols x maxRows that keeps the photo's shape,
// never bigger than the source.
func (a *artPiece) fit(maxCols, maxRows int) (cols, rows int) {
	rows = min(maxRows, a.rows)
	cols = (rows*a.cols + a.rows/2) / a.rows
	if cols > maxCols {
		cols = maxCols
		rows = (cols*a.rows + a.cols/2) / a.cols
	}
	return max(cols, 1), max(rows, 1)
}

func loadArt() error {
	entries, err := artFS.ReadDir("art")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json.gz") {
			continue
		}
		f, err := artFS.Open("art/" + e.Name())
		if err != nil {
			return err
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		var raw struct {
			Cols, Rows int
			Lum        []string `json:"lum"`
			RGB        []string `json:"rgb"`
		}
		err = json.NewDecoder(zr).Decode(&raw)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		p := &artPiece{cols: raw.Cols, rows: raw.Rows, cache: map[artKey][]string{}}
		for i := range raw.Lum {
			lum, err1 := base64.StdEncoding.DecodeString(raw.Lum[i])
			rgb, err2 := base64.StdEncoding.DecodeString(raw.RGB[i])
			if err1 != nil || err2 != nil || len(lum) != p.cols*p.rows || len(rgb) != 3*p.cols*p.rows {
				return fmt.Errorf("%s: frame %d is malformed", e.Name(), i)
			}
			p.lum = append(p.lum, lum)
			p.rgb = append(p.rgb, rgb)
		}
		art[strings.TrimSuffix(e.Name(), ".json.gz")] = p
	}
	return nil
}

// rendered returns one frame at cols x rows, coloured for the client's profile.
// Each (profile, size, frame) is built once and shared by every session, so
// playback costs nothing after the first loop.
func (a *artPiece) rendered(p termenv.Profile, cols, rows, frame int) []string {
	k := artKey{p, cols, rows, frame}
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.cache[k]; ok {
		return r
	}

	// average each target cell's block of source cells
	lum := make([]float64, cols*rows)
	rgb := make([][3]float64, cols*rows)
	for y := 0; y < rows; y++ {
		y0, y1 := y*a.rows/rows, max((y+1)*a.rows/rows, y*a.rows/rows+1)
		for x := 0; x < cols; x++ {
			x0, x1 := x*a.cols/cols, max((x+1)*a.cols/cols, x*a.cols/cols+1)
			var l, r, g, b float64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					i := sy*a.cols + sx
					l += float64(a.lum[frame][i])
					r += float64(a.rgb[frame][3*i])
					g += float64(a.rgb[frame][3*i+1])
					b += float64(a.rgb[frame][3*i+2])
				}
			}
			n := float64((y1 - y0) * (x1 - x0))
			lum[y*cols+x] = l / n
			rgb[y*cols+x] = [3]float64{r / n, g / n, b / n}
		}
	}
	// averaging softens edges, so sharpen a little when shrinking
	if cols < a.cols {
		lum = sharpen(lum, cols, rows, 0.6)
	}

	out := make([]string, rows)
	for y := 0; y < rows; y++ {
		var sb strings.Builder
		last := ""
		for x := 0; x < cols; x++ {
			i := y*cols + x
			ch := ramp[int(clampF(lum[i]/255, 0, 1)*float64(len(ramp)-1)+0.5)]
			if p != termenv.Ascii {
				if seq := colorSeq(p, rgb[i]); seq != last {
					sb.WriteString("\x1b[" + seq + "m")
					last = seq
				}
			}
			sb.WriteByte(ch)
		}
		if p != termenv.Ascii {
			sb.WriteString("\x1b[0m")
		}
		out[y] = sb.String()
	}

	a.cache[k] = out
	a.order = append(a.order, k)
	for _, r := range out {
		a.cacheSize += len(r)
	}
	for a.cacheSize > artCacheBytes && len(a.order) > 1 {
		old := a.order[0]
		a.order = a.order[1:]
		for _, r := range a.cache[old] {
			a.cacheSize -= len(r)
		}
		delete(a.cache, old)
	}
	return out
}

func sharpen(v []float64, cols, rows int, amount float64) []float64 {
	out := make([]float64, len(v))
	at := func(x, y int) float64 { return v[clamp(y, 0, rows-1)*cols+clamp(x, 0, cols-1)] }
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			blur := (at(x-1, y) + at(x+1, y) + at(x, y-1) + at(x, y+1) + at(x, y)) / 5
			out[y*cols+x] = at(x, y) + amount*(at(x, y)-blur)
		}
	}
	return out
}

func clampF(v, lo, hi float64) float64 { return max(lo, min(v, hi)) }

// colorSeq lifts a cell's colour so dark cells still read on black (the
// character already carries the brightness), then converts it for the client.
// Non-truecolour conversions are cached on a 5-bit grid, shared by everyone.
var (
	colorSeqMu    sync.Mutex
	colorSeqCache = map[termenv.Profile]map[uint16]string{}
)

func colorSeq(p termenv.Profile, c [3]float64) string {
	m := max(c[0], c[1], c[2], 1)
	k := (150 + 105*m/255) / m
	r, g, b := min(255, int(c[0]*k+0.5)), min(255, int(c[1]*k+0.5)), min(255, int(c[2]*k+0.5))
	if p == termenv.TrueColor {
		return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
	}
	key := uint16(r>>3)<<10 | uint16(g>>3)<<5 | uint16(b>>3)
	colorSeqMu.Lock()
	defer colorSeqMu.Unlock()
	if colorSeqCache[p] == nil {
		colorSeqCache[p] = map[uint16]string{}
	}
	if s, ok := colorSeqCache[p][key]; ok {
		return s
	}
	s := p.Color(fmt.Sprintf("#%02x%02x%02x", r&^7|4, g&^7|4, b&^7|4)).Sequence(false)
	colorSeqCache[p][key] = s
	return s
}

// ── canvas ───────────────────────────────────────────────────────────────────
//
// Pages draw into a fixed grid of cells, then each row is written out as runs
// of one style (and one link), so the layout is exact at any terminal size.

const (
	stNone = iota
	stDim
	stFg
	stFgB
	stFooter
	stR
	stRB
	stP
	stPB
	stG
	stGB
	stT
	stTB
	stCap
	stSel
	stSelSub
	stSelT
	stLink
	stStale
	stWarn
	stCount
)

func (m model) styles() []lipgloss.Style {
	r := m.renderer
	s := make([]lipgloss.Style, stCount)
	s[stNone] = r.NewStyle()
	s[stDim] = r.NewStyle().Foreground(lipgloss.Color("#7a7a7a"))
	s[stFg] = r.NewStyle().Foreground(lipgloss.Color("#d0d0d0"))
	s[stFgB] = r.NewStyle().Foreground(lipgloss.Color("#ffffff")).Bold(true)
	s[stFooter] = r.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#1a1a1a"))
	s[stR] = r.NewStyle().Foreground(pageColors[0])
	s[stRB] = r.NewStyle().Foreground(pageColors[0]).Bold(true)
	s[stP] = r.NewStyle().Foreground(lipgloss.Color("#8a5cc7"))
	s[stPB] = r.NewStyle().Foreground(lipgloss.Color("#a77bea")).Bold(true)
	s[stG] = r.NewStyle().Foreground(pageColors[2])
	s[stGB] = r.NewStyle().Foreground(pageColors[2]).Bold(true)
	s[stT] = r.NewStyle().Foreground(pageColors[3])
	s[stTB] = r.NewStyle().Foreground(lipgloss.Color("#16c0c0")).Bold(true)
	s[stCap] = r.NewStyle().Foreground(lipgloss.Color("#ff6b6b")).Italic(true)
	s[stSel] = r.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(pageColors[1]).Bold(true)
	s[stSelSub] = r.NewStyle().Foreground(lipgloss.Color("#e8dcff")).Background(pageColors[1])
	s[stSelT] = r.NewStyle().Foreground(lipgloss.Color("#001010")).Background(pageColors[3])
	s[stLink] = r.NewStyle().Foreground(lipgloss.Color("#8ab4f8")).Underline(true)
	s[stStale] = r.NewStyle().Foreground(lipgloss.Color("#5a5a5a"))
	s[stWarn] = r.NewStyle().Foreground(lipgloss.Color("#000000")).Background(pageColors[2])
	return s
}

type cell struct {
	r     rune
	st    int
	link  string
	raw   string // pre-rendered run starting here (art rows)
	rawW  int
	cover bool // part of a raw run
}

type canvas struct {
	w, h  int
	cells [][]cell
}

func newCanvas(w, h int) *canvas {
	c := &canvas{w: w, h: h, cells: make([][]cell, h)}
	for y := range c.cells {
		c.cells[y] = make([]cell, w)
		for x := range c.cells[y] {
			c.cells[y][x].r = ' '
		}
	}
	return c
}

func (c *canvas) putLink(x, y int, s string, st int, link string) {
	if y < 0 || y >= c.h {
		return
	}
	for _, r := range s {
		if x >= 0 && x < c.w {
			c.cells[y][x] = cell{r: r, st: st, link: link}
		}
		x++
	}
}

func (c *canvas) put(x, y int, s string, st int) { c.putLink(x, y, s, st, "") }

func (c *canvas) putRaw(x, y, width int, raw string) {
	if y < 0 || y >= c.h || x < 0 || x+width > c.w {
		return
	}
	c.cells[y][x] = cell{raw: raw, rawW: width}
	for i := 1; i < width; i++ {
		c.cells[y][x+i] = cell{cover: true}
	}
}

func (c *canvas) render(styles []lipgloss.Style) string {
	var b strings.Builder
	for y, row := range c.cells {
		var run []rune
		st, link := -1, ""
		flush := func() {
			if len(run) == 0 {
				return
			}
			if link != "" {
				b.WriteString("\x1b]8;;" + link + "\x1b\\")
			}
			b.WriteString(styles[st].Render(string(run)))
			if link != "" {
				b.WriteString("\x1b]8;;\x1b\\")
			}
			run = run[:0]
		}
		for _, cl := range row {
			if cl.cover {
				continue
			}
			if cl.raw != "" {
				flush()
				b.WriteString(cl.raw)
				continue
			}
			if cl.st != st || cl.link != link {
				flush()
				st, link = cl.st, cl.link
			}
			run = append(run, cl.r)
		}
		flush()
		if y < c.h-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ── small helpers ────────────────────────────────────────────────────────────

func strWidth(s string) int { return lipgloss.Width(s) }

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if strWidth(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && strWidth(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func padRight(s string, w int) string {
	return s + strings.Repeat(" ", max(0, w-strWidth(s)))
}

func wrap(s string, w int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case strWidth(line)+1+strWidth(word) <= w:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

func firstN[T any](s []T, n int) []T {
	if n < 0 {
		n = 0
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func mmss(ms int) string { return fmt.Sprintf("%d:%02d", ms/60000, ms/1000%60) }

// stars ports ratingToStars from misc.js; terminals have no half-star glyph, so ½
func stars(r float64) string {
	full := int(r)
	half := r-float64(full) >= 0.5
	out := strings.Repeat("★", full)
	empty := 5 - full
	if half {
		out += "½"
		empty--
	}
	return out + strings.Repeat("☆", max(0, empty))
}

var months = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}

// home is where the listening and watching happens; the VM itself runs on UTC
var home = func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.Local
}()

// shortDate turns 2026-10-26 into "oct 26"
func shortDate(d string) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s %d", months[t.Month()-1], t.Day())
}

func longDate(t time.Time) string {
	t = t.In(home)
	return fmt.Sprintf("%d %s %d", t.Day(), months[t.Month()-1], t.Year())
}

func timeAgo(t time.Time) string {
	day := func(t time.Time) time.Time {
		t = t.In(home)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, home)
	}
	days := int(day(time.Now()).Sub(day(t)).Hours() / 24)
	plural := func(n int, unit string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s ago", n, unit)
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	case days < 7:
		return plural(days, "day")
	case days < 30:
		return plural(days/7, "week")
	case days < 365:
		return plural(days/30, "month")
	}
	return plural(days/365, "year")
}

func times(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
}

// ── server ───────────────────────────────────────────────────────────────────

// maxSessions keeps the free VM's bandwidth for the people already connected
const maxSessions = 30

var activeSessions atomic.Int32

func limitSessions(next ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		if activeSessions.Add(1) > maxSessions {
			activeSessions.Add(-1)
			wish.Println(s, "the portfolio is busy right now, try again in a minute (or visit https://vedsite.com)")
			return
		}
		defer activeSessions.Add(-1)
		next(s)
	}
}

func main() {
	host := "0.0.0.0"
	port := 22
	if p, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
		port = p
	}

	if err := loadArt(); err != nil {
		log.Error("Could not load art", "error", err)
		os.Exit(1)
	}

	srv, err := wish.NewServer(
		wish.WithAddress(fmt.Sprintf("%s:%d", host, port)),
		wish.WithHostKeyPath(".ssh/host_ed25519"),
		wish.WithMiddleware(
			bubbletea.Middleware(func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
				go notifyVisitor(s)
				sess := &session{}
				go func() {
					<-s.Context().Done()
					sess.setMisc(false)
				}()
				renderer := bubbletea.MakeRenderer(s)
				return initialModel(renderer, sess), []tea.ProgramOption{tea.WithAltScreen()}
			}),
			limitSessions,
		),
	)
	if err != nil {
		log.Error("Could not create server", "error", err)
		os.Exit(1)
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	log.Info("Starting SSH server", "host", host, "port", port)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Error("Server error", "error", err)
		}
	}()

	<-done
	log.Info("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("Shutdown error", "error", err)
	}
}
