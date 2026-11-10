package main

// Designed image boards, one per main playlist (2v2 / 3v3 / 1v1).
// Minimal information only — position, name, rank, rating — set in a
// clean table on a deep-navy board, in the same visual family as the
// rl-shop-bot boards (Michroma titles, Noto Sans JP text).
//
// Fonts are embedded (Noto Sans JP: OFL, Michroma: OFL — license
// texts in assets/) so Actions and local runs render identically.

import (
	"bytes"
	"embed"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"sync"
	"time"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
)

//go:embed assets/NotoSansJP-Regular.ttf
var fontRegular []byte

//go:embed assets/NotoSansJP-Bold.ttf
var fontBold []byte

//go:embed assets/Michroma-Regular.ttf
var fontMichroma []byte

var parsedFonts sync.Map // string -> *truetype.Font

func loadFont(data []byte, key string) (*truetype.Font, error) {
	if f, ok := parsedFonts.Load(key); ok {
		return f.(*truetype.Font), nil
	}
	f, err := truetype.Parse(data)
	if err != nil {
		return nil, err
	}
	parsedFonts.Store(key, f)
	return f, nil
}

func fontFace(kind string, size float64) (font.Face, error) {
	var data []byte
	switch kind {
	case "bold":
		data = fontBold
	case "michroma":
		data = fontMichroma
	default:
		data = fontRegular
	}
	f, err := loadFont(data, kind)
	if err != nil {
		return nil, err
	}
	return truetype.NewFace(f, &truetype.Options{Size: size, DPI: 72, Hinting: font.HintingFull}), nil
}

func setFont(dc *gg.Context, kind string, size float64) error {
	face, err := fontFace(kind, size)
	if err != nil {
		return err
	}
	dc.SetFontFace(face)
	return nil
}

func rgb(hex int) (float64, float64, float64) {
	return float64((hex>>16)&0xFF) / 255, float64((hex>>8)&0xFF) / 255, float64(hex&0xFF) / 255
}

func setColor(dc *gg.Context, hex int) {
	r, g, b := rgb(hex)
	dc.SetRGB(r, g, b)
}

// truncateToWidth shortens s with an ellipsis so it fits maxW in the
// context's current font.
func truncateToWidth(dc *gg.Context, s string, maxW float64) string {
	if w, _ := dc.MeasureString(s); w <= maxW {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 {
		runes = runes[:len(runes)-1]
		cand := string(runes) + "…"
		if w, _ := dc.MeasureString(cand); w <= maxW {
			return cand
		}
	}
	return "…"
}

// boardRow is one ranking line: the minimum the board shows.
type boardRow struct {
	Name     string
	Tier     int
	Division int
	MMR      int
	Matches  int // sort tiebreak only, not displayed
}

// imgBoard is one rendered board message.
type imgBoard struct {
	Filename string
	Bytes    []byte
	Hash     string
}

const (
	boardW   = 1080.0
	boardPad = 54.0
	headerH  = 140.0
	rowH     = 76.0
	footerH  = 58.0
	// rowsPerBoard is both the split size AND the fixed board
	// height in rows: every board renders at exactly this many
	// rows tall (padded when a mode has fewer active members),
	// so all boards share one size and aspect ratio no matter
	// how the headcount changes.
	rowsPerBoard = 12
)

var posColors = map[int]int{1: 0xF2C14E, 2: 0xC7CEDA, 3: 0xD89A6A}

// renderBoard draws one board image for a chunk of rows of one
// mode. startRank is the 1-based position of the first row (it
// exceeds 1 on continuation boards of a split ranking).
func renderBoard(mode string, rows []boardRow, startRank int, updated time.Time) ([]byte, error) {
	n := len(rows)
	// Fixed canvas height (see rowsPerBoard): shorter rankings
	// simply leave empty board space above the footer.
	h := int(headerH + rowsPerBoard*rowH + footerH)
	dc := gg.NewContext(int(boardW), h)

	// Background: vertical navy gradient.
	grad := gg.NewLinearGradient(0, 0, 0, float64(h))
	grad.AddColorStop(0, colorFromHex(0x111C33))
	grad.AddColorStop(1, colorFromHex(0x0A0F1E))
	dc.SetFillStyle(grad)
	dc.DrawRectangle(0, 0, boardW, float64(h))
	dc.Fill()

	// Header: gradient gold bar + mode title + small brand.
	bar := gg.NewLinearGradient(0, 46, 0, 46+72)
	bar.AddColorStop(0, colorFromHex(0xF2C14E))
	bar.AddColorStop(1, colorFromHex(0xC9932B))
	dc.SetFillStyle(bar)
	dc.DrawRectangle(boardPad, 46, 10, 72)
	dc.Fill()
	if err := setFont(dc, "michroma", 56); err != nil {
		return nil, err
	}
	setColor(dc, 0xF4F6FA)
	dc.DrawStringAnchored(mode+" RANKING", boardPad+38, 84, 0, 0.5)
	if err := setFont(dc, "michroma", 20); err != nil {
		return nil, err
	}
	setColor(dc, 0x7787A3)
	dc.DrawStringAnchored("ROCKET LEAGUE", boardW-boardPad, 84, 1, 0.5)
	// Gold rule under the header.
	rule := gg.NewLinearGradient(boardPad, 0, boardW-boardPad, 0)
	rule.AddColorStop(0, colorFromHex(0xE8B93C))
	rule.AddColorStop(1, colorFromHexA(0xE8B93C, 0))
	dc.SetFillStyle(rule)
	dc.DrawRectangle(boardPad, headerH-2, boardW-2*boardPad, 2)
	dc.Fill()

	if n == 0 {
		if err := setFont(dc, "regular", 32); err != nil {
			return nil, err
		}
		setColor(dc, 0xA9BAD2)
		dc.DrawStringAnchored("今季プレイヤーなし", boardW/2, headerH+(rowsPerBoard*rowH)/2, 0.5, 0.5)
	} else {
		for i, row := range rows {
			y0 := headerH + float64(i)*rowH
			cy := y0 + rowH/2
			if i%2 == 0 {
				dc.SetRGBA(1, 1, 1, 0.028)
				dc.DrawRectangle(0, y0, boardW, rowH)
				dc.Fill()
			}
			// Position.
			rank := startRank + i
			// Podium accent: a slim bar in the position colour
			// along the left edge for the top 3.
			if c, ok := posColors[rank]; ok {
				setColor(dc, c)
				dc.DrawRectangle(0, y0, 5, rowH)
				dc.Fill()
			}
			if err := setFont(dc, "bold", 38); err != nil {
				return nil, err
			}
			if c, ok := posColors[rank]; ok {
				setColor(dc, c)
			} else {
				setColor(dc, 0x8E9BB5)
			}
			dc.DrawStringAnchored(fmt.Sprintf("%d", rank), boardPad+58, cy, 1, 0.5)
			// Tier label is measured first: the rank text always
			// stays fully visible, and the name truncates instead.
			if err := setFont(dc, "regular", 30); err != nil {
				return nil, err
			}
			labelFull := tierLabel(row.Tier, row.Division)
			labelW, _ := dc.MeasureString(labelFull)
			// Right zone holds [rank icon][MMR]; text stops before it.
			const mmrZoneX = boardW - boardPad - 245
			nameX := boardPad + 88
			nameMaxW := mmrZoneX - 26 - labelW - nameX
			if nameMaxW < 120 {
				nameMaxW = 120
			}
			// Name.
			if err := setFont(dc, "bold", 41); err != nil {
				return nil, err
			}
			setColor(dc, 0xF4F6FA)
			name := truncateToWidth(dc, row.Name, nameMaxW)
			dc.DrawStringAnchored(name, nameX, cy, 0, 0.5)
			nameW, _ := dc.MeasureString(name)
			// Tier label.
			if err := setFont(dc, "regular", 30); err != nil {
				return nil, err
			}
			setColor(dc, 0xB7C2D6)
			label := truncateToWidth(dc, labelFull, mmrZoneX-(nameX+nameW+26))
			dc.DrawStringAnchored(label, nameX+nameW+26, cy, 0, 0.5)
			// Rating, with the real rank icon beside it.
			if err := setFont(dc, "bold", 45); err != nil {
				return nil, err
			}
			setColor(dc, 0xF4F6FA)
			mmrText := fmt.Sprintf("%d", row.MMR)
			dc.DrawStringAnchored(mmrText, boardW-boardPad, cy, 1, 0.5)
			if icon := rankIcon(row.Tier); icon != nil {
				mmrW, _ := dc.MeasureString(mmrText)
				iconRight := boardW - boardPad - mmrW - 16
				dc.DrawImageAnchored(icon, int(iconRight), int(cy), 1, 0.5)
			}
			// Divider.
			dc.SetRGBA(1, 1, 1, 0.05)
			dc.DrawRectangle(boardPad, y0+rowH-1, boardW-2*boardPad, 1)
			dc.Fill()
		}
	}

	// Footer: a mirrored gold hairline echoes the header rule,
	// and the texts are set in Michroma at regular weight — the
	// font's wide letterforms do the styling, no bold needed.
	frule := gg.NewLinearGradient(boardPad, 0, boardW-boardPad, 0)
	frule.AddColorStop(0, colorFromHexA(0xE8B93C, 0))
	frule.AddColorStop(1, colorFromHex(0xE8B93C))
	dc.SetFillStyle(frule)
	dc.DrawRectangle(boardPad, float64(h)-footerH, boardW-2*boardPad, 2)
	dc.Fill()
	jst := time.FixedZone("JST", 9*60*60)
	if err := setFont(dc, "michroma", 19); err != nil {
		return nil, err
	}
	setColor(dc, 0x7787A3)
	dc.DrawStringAnchored("RL RANK LIVE", boardPad, float64(h)-26, 0, 0.5)
	if err := setFont(dc, "michroma", 24); err != nil {
		return nil, err
	}
	setColor(dc, 0xCBD6E8)
	dc.DrawStringAnchored(updated.In(jst).Format("2006.01.02 15:04")+" JST", boardW-boardPad, float64(h)-26, 1, 0.5)

	// Framing stroke around the whole card.
	dc.SetRGBA(1, 1, 1, 0.07)
	dc.SetLineWidth(2)
	dc.DrawRectangle(1, 1, boardW-2, float64(h)-2)
	dc.Stroke()

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dc.Image(), &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ------------------------------------------------------------ rank icons

// The real in-game rank icons, one per Tier (0=Unranked .. 22=SSL),
// sourced from the Rocket League Wiki (same asset source the shop
// bot uses for the Credits icon). Icons differ per Tier — Bronze I
// vs II vs III are different images — exactly as in game. (Within a
// Tier, Divisions I–IV share the icon in game too; the division is
// text there and here.)
//
//go:embed assets/ranks/tier*.png
var rankIconFS embed.FS

const rankIconSize = 54

var rankIcons [23]image.Image
var rankIconsErr error
var rankIconsOnce sync.Once

func loadRankIcons() {
	for tier := 0; tier < len(rankIcons); tier++ {
		name := fmt.Sprintf("assets/ranks/tier%02d.png", tier)
		f, err := rankIconFS.Open(name)
		if err != nil {
			rankIconsErr = err
			return
		}
		img, err := png.Decode(f)
		f.Close()
		if err != nil {
			rankIconsErr = err
			return
		}
		scaled := image.NewRGBA(image.Rect(0, 0, rankIconSize, rankIconSize))
		draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), img, img.Bounds(), draw.Over, nil)
		rankIcons[tier] = scaled
	}
}

func rankIcon(tier int) image.Image {
	rankIconsOnce.Do(loadRankIcons)
	if rankIconsErr != nil || tier < 0 || tier >= len(rankIcons) {
		return nil
	}
	return rankIcons[tier]
}

// ---------------------------------------------------------------

func colorFromHex(hex int) color.Color {
	r, g, b := rgb(hex)
	return color.RGBA{uint8(r * 255), uint8(g * 255), uint8(b * 255), 255}
}

func colorFromHexA(hex int, a float64) color.Color {
	r, g, b := rgb(hex)
	return color.RGBA{uint8(r * 255), uint8(g * 255), uint8(b * 255), uint8(a * 255)}
}
