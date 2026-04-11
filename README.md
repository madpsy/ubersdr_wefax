# ubersdr_wefax

Automated HF weather-fax (WEFAX/radiofax) receiver for [UberSDR](https://ubersdr.org) — connects to a remote UberSDR instance, tunes one or more HF channels simultaneously, decodes the FM-subcarrier fax signal in pure Go, and serves decoded images in a live web gallery.

---

## How it works

```
UberSDR (remote SDR) --► ubersdr_wefax (Go) --► decoded fax images
                                 |
                                 └──► web gallery  http://<host>:6094
```

- **`ubersdr_wefax`** — Go service that connects to UberSDR via WebSocket, streams demodulated audio, and runs a built-in WEFAX decoder per channel
- **WEFAX decoder** — FM demodulation, 17-tap FIR low-pass filter, horizontal phasing, START/STOP tone detection (DFT at 300/450 Hz), grayscale image assembly
- **Web gallery** — live scrolling fax strip with per-channel selector, gallery of completed images with SNR metadata, audio preview; served on port 6094

Multiple frequencies are decoded in parallel — each channel runs its own independent WebSocket connection and decoder.

---

## Quick start (Docker — recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_wefax/main/install.sh | bash
```

This will:
1. Create `~/ubersdr/wefax/` and download `docker-compose.yml` and helper scripts
2. Create the `wefax-images/` output directory
3. Pull the latest `madpsy/ubersdr_wefax` image
4. Start the service

Then edit `~/ubersdr/wefax/docker-compose.yml` to set your UberSDR URL and channels, and run `./restart.sh`.

Open `http://<host>:6094` to view the web gallery.

---

## Configuration

All configuration is via environment variables in `docker-compose.yml`:

| Variable | Default | Description |
|----------|---------|-------------|
| `UBERSDR_URL` | `ws://ubersdr:8080/ws` | UberSDR WebSocket URL |
| `UBERSDR_CHANNELS` | _(see below)_ | Comma-separated `freq:mode` pairs, e.g. `7880000:usb,13882500:usb` |
| `UBERSDR_PASS` | _(empty)_ | UberSDR bypass password |
| `OUTPUT_DIR` | `/data` | Output directory for images inside the container |
| `WEB_PORT` | `6094` | Web gallery port |
| `LPM` | `120` | Lines per minute: `120` (standard) or `60` (slow) |
| `IMAGE_WIDTH` | `1809` | Image width in pixels: `1809` (IOC-576) or `904` (IOC-288) |
| `NO_PHASING` | `0` | Set to `1` to disable horizontal phasing sync |
| `NO_AUTOSTOP` | `0` | Set to `1` to disable auto-stop on STOP tone |
| `NO_AUTOSTART` | `0` | Set to `1` to disable auto-start on START tone |

### Supported modes

Any mode supported by UberSDR's audio demodulator: `usb`, `lsb`, `am`, `fm`, etc.
WEFAX transmissions are almost always received on `usb`.

---

## Default frequencies

The default `UBERSDR_CHANNELS` includes a cross-regional selection of well-known HF wefax stations.
Edit `docker-compose.yml` to match the frequencies receivable at your location.

| Station | Frequencies (Hz) | Region |
|---------|-----------------|--------|
| NOAA (USA) | 4317900, 8503900, 12788500, 17151200 | Americas |
| DWD (Germany) | 3855000, 7880000, 13882500 | Europe |
| GYA (UK/Northwood) | 2618500, 4610000, 11086500 | Europe / North Atlantic |
| JMH (Japan) | 3622500, 7795000, 13988500, 22527000 | Asia / Pacific |
| FFU/FUG (France) | 3855000, 7710000, 13155000 | Europe / Mediterranean |
| BMF (Taiwan) | 4616000, 8140000, 12230000 | Asia / Pacific |

---

## Helper scripts

After running `install.sh`, the following scripts are available in `~/ubersdr/wefax/`:

| Script | Action |
|--------|--------|
| `./start.sh` | Start the service |
| `./stop.sh` | Stop the service |
| `./restart.sh` | Restart the service (apply config changes) |
| `./update.sh` | Pull the latest image and restart |

---

## Building from source

### Docker image

```bash
./docker.sh build          # build madpsy/ubersdr_wefax:latest
./docker.sh push           # build and push to Docker Hub
./docker.sh run            # run locally (uses env vars)
```

Override the image name:
```bash
IMAGE=myrepo/ubersdr_wefax:dev ./docker.sh build
```

### Go binary

Requires Go 1.21+

```bash
go build -o ubersdr_wefax ./...
./ubersdr_wefax \
    -url ws://sdr.example.com/ws \
    -channel 7880000:usb \
    -channel 13882500:usb \
    -listen :6094 \
    -output ./images
```

All flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-url` | _(required)_ | UberSDR WebSocket URL |
| `-channel` | _(required, repeatable)_ | `freq:mode` to decode, e.g. `7880000:usb` |
| `-password` | _(empty)_ | UberSDR bypass password |
| `-listen` | `:8080` | HTTP listen address |
| `-output` | `./images` | Directory to save decoded images |
| `-lpm` | `120` | Lines per minute |
| `-width` | `1809` | Image width in pixels |
| `-no-phasing` | _(off)_ | Disable horizontal phasing sync |
| `-no-autostop` | _(off)_ | Disable auto-stop on STOP tone |
| `-no-autostart` | _(off)_ | Disable auto-start on START tone |

---

## Web gallery

Open `http://<host>:6094` in a browser to view:

- Live scrolling fax strip — rows appear in real time as they are decoded
- Per-channel selector to monitor individual frequencies
- Gallery of completed images with frequency, timestamp, dimensions, and SNR metadata
- Audio preview stream for any active channel

---

## Output files

Each completed fax image produces three files in the output directory:

| File | Description |
|------|-------------|
| `<timestamp>_<id>.png` | Full-resolution grayscale PNG |
| `<timestamp>_<id>_thumb.png` | Thumbnail (max 400 px wide) |
| `<timestamp>_<id>.json` | Sidecar metadata (frequency, mode, timestamps, SNR stats) |

---

## Volumes

| Path (container) | Description |
|-----------------|-------------|
| `/data` | Decoded images and JSON sidecar metadata |

Mapped to `./wefax-images` on the host by default (created by `install.sh`).

---

## Ports

| Port | Description |
|------|-------------|
| `6094` | Web gallery (HTTP) |

---

## License

MIT
