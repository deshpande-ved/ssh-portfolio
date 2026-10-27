# ssh-portfolio

My website, [vedsite.com](https://vedsite.com), in your terminal:

```
ssh portfolio.vedsite.com
```

The home screen is the site's bouncing shapes over a tiled `vedsite` background.
Press `1`–`4` to open a page:

- **about**: bio, links, and four photos as colour ASCII art (the buzz cut one
  plays all 52 weekly photos)
- **projects**: every project on the site, with a short write-up and links
- **experience**: work, leadership, education, languages and tools
- **misc**: what I'm listening to and watching, live from `api.vedsite.com`,
  plus the two search boxes: "have i heard it?" and "have i seen it?"

Every page lists its keys in the footer. Links are terminal hyperlinks, so most
terminals open them in your browser on click (⌘-click on macOS).

## Architecture

```
your terminal → Cloudflare DNS (A record, DNS-only) → Oracle Cloud VM, port 22 → Wish → Bubble Tea
your browser  → Cloudflare DNS (same record)         → Oracle Cloud VM, ports 80/443 → Caddy → 302 to vedsite.com/projects/ssh
```

- One Go binary runs natively on an Oracle Cloud free-tier VM. SSH can't go
  through Cloudflare's proxy on the free tier, so the record is DNS-only.
- Because the record is DNS-only, Cloudflare can't redirect browsers either, so
  [Caddy](https://caddyserver.com) on the same VM does it: opening
  `portfolio.vedsite.com` in a browser (http or https, any path) sends you to the
  project page at [vedsite.com/projects/ssh](https://vedsite.com/projects/ssh).
  Caddy gets and renews its own Let's Encrypt certificate and never touches port 22.
- [Wish](https://github.com/charmbracelet/wish) is the SSH server. Each
  connection gets its own [Bubble Tea](https://github.com/charmbracelet/bubbletea)
  program, styled with [Lip Gloss](https://github.com/charmbracelet/lipgloss).
- Pages draw into a fixed grid of cells that is written out in runs of one
  style, so layouts are exact at any size from 80×24 up. Smaller terminals get
  a "make the window bigger" screen.
- The misc page reads from one shared cache. It only polls the API while
  someone is on that page, so a hundred visitors cost the API the same as one.
  If the API is down, the last good data stays up, marked stale.
- Searches wait 150 ms after the last keystroke and drop out-of-date responses.
  Recent queries are cached for five minutes.
- The photos are prepared offline (see below) and embedded with `go:embed`.
  At runtime each one is scaled to the largest size that fits the visitor's
  terminal. Every size and colour profile (truecolour, 256, 16, none) is built
  once, cached and shared, so animation costs almost nothing per visitor.
- Sessions time out after 10 minutes, and there's a cap on how many can be
  connected at once.

Everything is in `main.go`.

## Run it locally

```
PORT=2222 go run .
ssh -p 2222 localhost      # in another terminal
```

The first run generates a throwaway host key in `.ssh/`, which is gitignored.

## Environment

| Variable          | Default | What it does |
|-------------------|---------|--------------|
| `PORT`            | `22`    | Port to listen on |
| `DISCORD_WEBHOOK` | unset   | When set, posts a short note per visit (IP and approximate location) to a Discord channel. Unset (e.g. locally), nothing is sent. |

## Build

The VM is x86-64 Linux, so cross-compile a static binary:

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ssh-portfolio-amd64 .
file ssh-portfolio-amd64   # ELF 64-bit, x86-64, statically linked
```

## Deploy

1. Copy the new binary to the VM **next to** the running one, never over it.
2. Stop the service, keep the old binary as a rollback copy, move the new one
   into place, start the service.
3. Check it's active and the logs are clean, then `ssh portfolio.vedsite.com`.
   There must be **no host-key warning**. If there is, something touched the
   host key: stop and roll back.
4. Rollback is the same swap in reverse.

**Host keys.** The server's host key is generated on the VM and only ever lives
there. It is never copied off the server, never overwritten by a deploy and
never committed. `.gitignore` and a pre-commit hook both block key files, the
built binary and `.env` files from being staged.

## Regenerating the art

The photos live in the website's repo. `art/make_art.py` crops, sharpens and
converts them (needs Python 3 and ffmpeg):

```
python3 art/make_art.py path/to/vedsite/media/about
```

It writes `art/<photo>.json.gz`: brightness and colour per terminal cell, at the
biggest size the page draws (up to 64 rows). `main.go` scales that down to fit,
picks each cell's character from a 70-step brightness ramp and keeps the colour
of its pixel.
