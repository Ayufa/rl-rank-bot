// rlrank: Rocket League rank live board for one tracked player.
//
// Fully programmatic pipeline (no manual fetching):
//  1. EGS refresh-token login (a DEDICATED login for this bot only,
//     created once via `auth-code`; never shared with rl-shop-bot).
//  2. Epic account-service displayName lookup: player name -> account ID.
//  3. PsyNet (via dank/rlapi) GetPlayersSkills for that account ID:
//     per-playlist Tier / Division / MMR / WinStreak / MatchesPlayed.
//  4. Discord Webhook: post one "live board" message, then EDIT that
//     same message on later runs when a value changed.
//
// Usage:
//
//	rlrank auth-url            print the Epic login URL for `auth-code`
//	rlrank auth-code <code>    exchange an authorization code, save token
//	rlrank run [--post]        fetch once; --post also updates Discord
//
// Environment:
//
//	RL_RANK_TOKEN_FILE    refresh-token file (default: token.txt)
//	RL_RANK_REFRESH_TOKEN seed refresh token (first run only)
//	RL_RANK_STATE_FILE    board state file (default: state/board.json)
//	RL_TARGET_NAME        tracked Epic display name (default: Αyufa)
//	DISCORD_WEBHOOK_URL   webhook for --post
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dank/rlapi"
)

// ---------------------------------------------------------------- tiers

var tierNames = []string{
	"Unranked",
	"Bronze I", "Bronze II", "Bronze III",
	"Silver I", "Silver II", "Silver III",
	"Gold I", "Gold II", "Gold III",
	"Platinum I", "Platinum II", "Platinum III",
	"Diamond I", "Diamond II", "Diamond III",
	"Champion I", "Champion II", "Champion III",
	"Grand Champion I", "Grand Champion II", "Grand Champion III",
	"Supersonic Legend",
}

var divisionRoman = []string{"I", "II", "III", "IV"}

func tierLabel(tier, division int) string {
	if tier < 0 || tier >= len(tierNames) {
		return fmt.Sprintf("Tier %d", tier)
	}
	name := tierNames[tier]
	// Unranked and Supersonic Legend have no divisions.
	if tier == 0 || tier == len(tierNames)-1 {
		return name
	}
	if division >= 0 && division < len(divisionRoman) {
		return name + " Div " + divisionRoman[division]
	}
	return name
}

func tierColor(tier int) int {
	switch {
	case tier >= 22:
		return 0xF5F5F5 // Supersonic Legend: white
	case tier >= 19:
		return 0xE33E4E // Grand Champion: red
	case tier >= 16:
		return 0xB565D8 // Champion: purple
	case tier >= 13:
		return 0x3E7BDE // Diamond: blue
	case tier >= 10:
		return 0x35C4C9 // Platinum: cyan
	case tier >= 7:
		return 0xE8B93C // Gold
	case tier >= 4:
		return 0xA8B0B8 // Silver
	case tier >= 1:
		return 0xB0722E // Bronze
	default:
		return 0x808080
	}
}

// ------------------------------------------------------------ playlists

type playlistInfo struct {
	id    int
	label string
	main  bool
}

var playlistOrder = []playlistInfo{
	{11, "⭐ 2v2", true},
	{13, "3v3", true},
	{10, "1v1", true},
	{34, "Tournament", false},
	{23, "Hoops", false},
	{27, "Rumble", false},
	{28, "Dropshot", false},
	{29, "Snow Day", false},
}

// displayMMR converts a PsyNet skill into the rating the game shows.
// PsyNet's MMR field is the display rating when it is in display range;
// when only Mu is meaningful, the standard conversion is 20*Mu+100.
func displayMMR(s rlapi.Skill) int {
	if s.MMR >= 200 {
		return int(math.Round(s.MMR))
	}
	return int(math.Round(20*s.Mu + 100))
}

// ---------------------------------------------------------------- state

type skillSnapshot struct {
	Tier     int `json:"tier"`
	Division int `json:"division"`
	MMR      int `json:"mmr"`
	Matches  int `json:"matches"`
	Streak   int `json:"streak"`
}

type boardState struct {
	MessageID string                  `json:"message_id,omitempty"`
	AccountID string                  `json:"account_id,omitempty"`
	Prev      map[string]skillSnapshot `json:"prev,omitempty"`
	UpdatedAt string                  `json:"updated_at,omitempty"`
}

func stateFilePath() string {
	if p := os.Getenv("RL_RANK_STATE_FILE"); p != "" {
		return p
	}
	return filepath.Join("state", "board.json")
}

func loadState() boardState {
	var st boardState
	b, err := os.ReadFile(stateFilePath())
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Prev == nil {
		st.Prev = map[string]skillSnapshot{}
	}
	return st
}

func saveState(st boardState) error {
	p := stateFilePath()
	if dir := filepath.Dir(p); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// ---------------------------------------------------------------- token

func tokenFilePath() string {
	if p := os.Getenv("RL_RANK_TOKEN_FILE"); p != "" {
		return p
	}
	return "token.txt"
}

func readRefreshToken() (string, error) {
	if b, err := os.ReadFile(tokenFilePath()); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	if seed := strings.TrimSpace(os.Getenv("RL_RANK_REFRESH_TOKEN")); seed != "" {
		return seed, nil
	}
	return "", fmt.Errorf("no refresh token found (run `rlrank auth-code` once, or set RL_RANK_REFRESH_TOKEN)")
}

func writeRefreshToken(tok string) error {
	return os.WriteFile(tokenFilePath(), []byte(tok), 0o600)
}

// ------------------------------------------------------- Epic name lookup

type epicAccount struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// lookupByDisplayName resolves an Epic display name to an account using
// the Epic account service. This requires a USER access token: an
// app (client-credentials) token is rejected with
// errors.com.epicgames.common.missing_permission
// (account:public:account READ) — verified 2026-11-10.
func lookupByDisplayName(accessToken, name string) (*epicAccount, error) {
	hosts := []string{
		"account-public-service-prod03.ol.epicgames.com",
		"account-public-service-prod.ol.epicgames.com",
	}
	var lastErr error
	for _, host := range hosts {
		u := fmt.Sprintf("https://%s/account/api/public/account/displayName/%s", host, url.PathEscape(name))
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "bearer "+accessToken)
		req.Header.Set("User-Agent", "UELauncher/11.0.1-14907503+++Portal+Release-Live Windows/10.0.19041.1.256.64bit")
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var acc epicAccount
			if err := json.Unmarshal(body, &acc); err != nil {
				return nil, fmt.Errorf("parse lookup response: %w", err)
			}
			if acc.ID == "" {
				// Some responses use accountId instead of id.
				var alt struct {
					AccountID string `json:"accountId"`
				}
				_ = json.Unmarshal(body, &alt)
				acc.ID = alt.AccountID
			}
			if acc.ID == "" {
				return nil, fmt.Errorf("lookup returned no account id: %s", string(body))
			}
			return &acc, nil
		}
		lastErr = fmt.Errorf("%s: HTTP %d: %s", host, resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("no Epic account with display name %q", name)
		}
	}
	return nil, lastErr
}

// ---------------------------------------------------------------- login

// connect performs the dedicated EGS login and returns the token
// response (for the account-service lookup) plus a PsyNet RPC client.
func connect() (*rlapi.TokenResponse, *rlapi.PsyNetRPC, error) {
	egs := rlapi.NewEGS()
	tok, err := readRefreshToken()
	if err != nil {
		return nil, nil, err
	}
	auth, err := egs.AuthenticateWithRefreshToken(tok)
	if err != nil {
		return nil, nil, fmt.Errorf("EGS refresh failed (re-run `auth-code`): %w", err)
	}
	// Refresh tokens rotate: persist the new one immediately.
	if auth.RefreshToken != "" && auth.RefreshToken != tok {
		if err := writeRefreshToken(auth.RefreshToken); err != nil {
			return nil, nil, fmt.Errorf("persist rotated token: %w", err)
		}
	}
	code, err := egs.GetExchangeCode(auth.AccessToken)
	if err != nil {
		return nil, nil, fmt.Errorf("GetExchangeCode: %w", err)
	}
	eos, err := egs.ExchangeEOSToken(code)
	if err != nil {
		return nil, nil, fmt.Errorf("ExchangeEOSToken: %w", err)
	}
	psy := rlapi.NewPsyNet()
	rpc, err := psy.AuthPlayer(eos.AccessToken, eos.AccountID, auth.DisplayName)
	if err != nil {
		return nil, nil, fmt.Errorf("AuthPlayer: %w", err)
	}
	return auth, rpc, nil
}

// -------------------------------------------------------------- discord

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type embed struct {
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color"`
	Fields      []embedField `json:"fields,omitempty"`
	Footer      *embedFooter `json:"footer,omitempty"`
}

type embedFooter struct {
	Text string `json:"text"`
}

func webhookCall(method, url string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

// postOrEdit posts a new board message, or edits the recorded one.
func postOrEdit(webhook, messageID string, emb embed) (string, error) {
	payload := map[string]any{"username": "RL Rank Live", "embeds": []embed{emb}}
	if messageID != "" {
		status, out, err := webhookCall("PATCH", webhook+"/messages/"+messageID, payload)
		if err != nil {
			return "", err
		}
		if status == http.StatusOK {
			return messageID, nil
		}
		if status != http.StatusNotFound {
			return "", fmt.Errorf("edit board: HTTP %d: %s", status, string(out))
		}
		// Message was deleted in Discord: fall through and repost.
	}
	status, out, err := webhookCall("POST", webhook+"?wait=true", payload)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("post board: HTTP %d: %s", status, string(out))
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &msg); err != nil || msg.ID == "" {
		return "", fmt.Errorf("post board: no message id in response")
	}
	return msg.ID, nil
}

// ----------------------------------------------------------------- cmds

func cmdAuthURL() error {
	egs := rlapi.NewEGS()
	fmt.Println(egs.GetAuthURL())
	return nil
}

func cmdAuthCode(args []string) error {
	var code string
	if len(args) > 0 {
		code = strings.TrimSpace(args[0])
	} else {
		fmt.Print("Auth code: ")
		var line string
		_, _ = fmt.Scanln(&line)
		code = strings.TrimSpace(line)
	}
	if code == "" {
		return fmt.Errorf("empty auth code")
	}
	egs := rlapi.NewEGS()
	auth, err := egs.AuthenticateWithCode(code)
	if err != nil {
		return err
	}
	if auth.RefreshToken == "" {
		return fmt.Errorf("no refresh token returned")
	}
	if err := writeRefreshToken(auth.RefreshToken); err != nil {
		return err
	}
	fmt.Printf("Dedicated rank-bot login saved (account: %s)\n", auth.DisplayName)
	return nil
}

func cmdRun(post bool) error {
	targetName := os.Getenv("RL_TARGET_NAME")
	if targetName == "" {
		targetName = "Αyufa"
	}

	auth, rpc, err := connect()
	if err != nil {
		return err
	}
	defer rpc.Close()
	fmt.Printf("Logged in as %s\n", auth.DisplayName)

	acc, err := lookupByDisplayName(auth.AccessToken, targetName)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", targetName, err)
	}
	fmt.Printf("Resolved %q -> account %s (displayName %q)\n", targetName, acc.ID, acc.DisplayName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	targetID := rlapi.NewPlayerID(rlapi.PlatformEpic, acc.ID)
	players, err := rpc.GetPlayersSkills(ctx, []rlapi.PlayerID{targetID})
	if err != nil {
		return fmt.Errorf("GetPlayersSkills: %w", err)
	}
	if len(players) == 0 || len(players[0].Skills) == 0 {
		return fmt.Errorf("no skill data returned for %s", targetID)
	}
	skills := players[0].Skills

	// Raw dump for verification (Actions log / local stdout).
	if raw, err := json.Marshal(skills); err == nil {
		fmt.Printf("RAW skills: %s\n", string(raw))
	}

	// Optional name cross-check via profile.
	if profiles, err := rpc.GetProfiles(ctx, []rlapi.PlayerID{targetID}); err == nil && len(profiles) > 0 {
		fmt.Printf("Profile: name=%q presence=%q\n", profiles[0].PlayerName, profiles[0].PresenceState)
	}

	st := loadState()
	st.AccountID = acc.ID

	byPlaylist := map[int]rlapi.Skill{}
	for _, s := range skills {
		byPlaylist[s.Playlist] = s
	}

	bestTier := 0
	var fields []embedField
	changed := false
	for _, pi := range playlistOrder {
		s, ok := byPlaylist[pi.id]
		if !ok || (s.MatchesPlayed == 0 && s.Tier == 0) {
			continue
		}
		// Extra modes keep last season's tier with 0 matches played;
		// showing those would present a stale rank as current.
		if !pi.main && s.MatchesPlayed == 0 {
			continue
		}
		mmr := displayMMR(s)
		if pi.main && s.Tier > bestTier {
			bestTier = s.Tier
		}
		key := fmt.Sprintf("%d", pi.id)
		prev, hadPrev := st.Prev[key]
		delta := ""
		if hadPrev {
			if d := mmr - prev.MMR; d > 0 {
				delta = fmt.Sprintf(" ▲+%d", d)
				changed = true
			} else if d < 0 {
				delta = fmt.Sprintf(" ▼%d", d)
				changed = true
			}
			if prev.Tier != s.Tier || prev.Division != s.Division || prev.Matches != s.MatchesPlayed {
				changed = true
			}
		} else {
			changed = true
		}
		val := fmt.Sprintf("**%s**\n%d MMR%s\n%d試合", tierLabel(s.Tier, s.Division), mmr, delta, s.MatchesPlayed)
		if s.WinStreak >= 2 {
			val += fmt.Sprintf(" ／ %d連勝🔥", s.WinStreak)
		}
		fields = append(fields, embedField{Name: pi.label, Value: val, Inline: true})
		st.Prev[key] = skillSnapshot{Tier: s.Tier, Division: s.Division, MMR: mmr, Matches: s.MatchesPlayed, Streak: s.WinStreak}
	}
	if len(fields) == 0 {
		return fmt.Errorf("no displayable playlists for %s", targetName)
	}

	displayName := acc.DisplayName
	if displayName == "" {
		displayName = targetName
	}
	emb := embed{
		Title:       fmt.Sprintf("🏆 %s — ランク速報", displayName),
		Description: fmt.Sprintf("🕒 最終更新 <t:%d:R>\nデータ：Rocket League公式サーバー（PsyNet）から自動取得", time.Now().Unix()),
		Color:       tierColor(bestTier),
		Fields:      fields,
		Footer:      &embedFooter{Text: "RL Rank Live • プログラムが自動で取得・編集しています"},
	}

	if post {
		webhook := strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL"))
		if webhook == "" {
			return fmt.Errorf("DISCORD_WEBHOOK_URL is not set")
		}
		id, err := postOrEdit(webhook, st.MessageID, emb)
		if err != nil {
			return err
		}
		if id != st.MessageID {
			fmt.Printf("Posted board message %s\n", id)
			st.MessageID = id
		} else {
			fmt.Printf("Edited board message %s (changed=%v)\n", id, changed)
		}
	} else {
		fmt.Println("Verification mode: board not posted (run with --post to post/edit).")
	}

	st.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveState(st)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rlrank <auth-url|auth-code|run> [--post]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "auth-url":
		err = cmdAuthURL()
	case "auth-code":
		err = cmdAuthCode(os.Args[2:])
	case "run":
		post := false
		for _, a := range os.Args[2:] {
			if a == "--post" {
				post = true
			}
		}
		err = cmdRun(post)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
