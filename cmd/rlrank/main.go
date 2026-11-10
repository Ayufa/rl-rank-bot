// rlrank: Rocket League member ranking board for Discord.
//
// Fully programmatic pipeline (no manual fetching):
//  1. EGS refresh-token login (a DEDICATED login for this bot only,
//     created once via `auth-code`; never shared with rl-shop-bot).
//  2. Epic account-service displayName lookup: member name -> account
//     ID (cached in state; later runs resolve by ID, so renames do
//     not break tracking).
//  3. PsyNet (via dank/rlapi) GetPlayersSkills for ALL members in
//     batched calls: per-playlist Tier / Division / MMR / WinStreak /
//     MatchesPlayed.
//  4. Members are ranked by a representative playlist (2v2 first,
//     then 3v3, 1v1, then the highest-MMR active playlist) and shown
//     as one-line entries on Discord "ranking board" message(s) that
//     are EDITED in place on later runs.
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
//	RL_TARGETS_FILE       member list file (default: targets.txt)
//	RL_TARGET_NAME        fallback single member when no targets file
//	DISCORD_WEBHOOK_URL   webhook for --post
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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

const rankingColor = 0xE8B93C // gold

// ------------------------------------------------------------ playlists

type playlistInfo struct {
	id    int
	label string // detail label (kept from v1)
	short string // compact label for ranking lines
	main  bool
}

var playlistOrder = []playlistInfo{
	{11, "⭐ 2v2", "2v2", true},
	{13, "3v3", "3v3", true},
	{10, "1v1", "1v1", true},
	{34, "Tournament", "大会", false},
	{23, "Hoops", "Hoops", false},
	{27, "Rumble", "Rumble", false},
	{28, "Dropshot", "Dropshot", false},
	{29, "Snow Day", "Snow Day", false},
}

var playlistShort = func() map[int]string {
	m := map[int]string{}
	for _, pi := range playlistOrder {
		m[pi.id] = pi.short
	}
	return m
}()

// repPreference: order in which an active playlist (matches played
// this season > 0) becomes a member's representative playlist.
var repPreference = []int{11, 13, 10, 34, 27, 23, 28, 29}

// displayMMR converts a PsyNet skill into the rating the game shows.
// PsyNet's MMR field equals Mu in raw responses; the display rating
// is 20*Mu+100 (verified Tier-consistent on 2026-11-10).
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

type memberState struct {
	Name     string                   `json:"name,omitempty"`
	Prev     map[string]skillSnapshot `json:"prev,omitempty"`
	PrevRank int                      `json:"prev_rank,omitempty"`
}

type boardState struct {
	Version     int                     `json:"version,omitempty"`
	BoardIDs    []string                `json:"board_ids,omitempty"`
	BoardHashes []string                `json:"board_hashes,omitempty"`
	Resolved    map[string]string       `json:"resolved,omitempty"` // target name -> account ID
	Members     map[string]*memberState `json:"members,omitempty"`  // account ID -> state
	UpdatedAt   string                  `json:"updated_at,omitempty"`

	// Legacy v1 fields (single-player board), migrated on load.
	LegacyMessageID string                   `json:"message_id,omitempty"`
	LegacyAccountID string                   `json:"account_id,omitempty"`
	LegacyPrev      map[string]skillSnapshot `json:"prev,omitempty"`
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
	if st.Resolved == nil {
		st.Resolved = map[string]string{}
	}
	if st.Members == nil {
		st.Members = map[string]*memberState{}
	}
	// Migrate the v1 single-player snapshot so the first ranked run
	// still shows MMR deltas for the original member.
	if st.LegacyAccountID != "" && len(st.LegacyPrev) > 0 {
		if _, ok := st.Members[st.LegacyAccountID]; !ok {
			st.Members[st.LegacyAccountID] = &memberState{Prev: st.LegacyPrev}
		}
		st.LegacyPrev = nil
		st.LegacyAccountID = ""
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

// ------------------------------------------------------------- targets

// loadTargets reads the member list: one Epic display name per line,
// blank lines and lines starting with '#' are ignored. Falls back to
// RL_TARGET_NAME / the historical default when the file is absent.
func loadTargets() ([]string, error) {
	path := os.Getenv("RL_TARGETS_FILE")
	if path == "" {
		path = "targets.txt"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		name := os.Getenv("RL_TARGET_NAME")
		if name == "" {
			name = "Αyufa"
		}
		return []string{name}, nil
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !seen[line] {
			seen[line] = true
			names = append(names, line)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s contains no member names", path)
	}
	return names, nil
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

// deleteMessage removes a webhook message; already-gone counts as ok.
func deleteMessage(webhook, messageID string) error {
	status, out, err := webhookCall("DELETE", webhook+"/messages/"+messageID, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("delete message %s: HTTP %d: %s", messageID, status, string(out))
}

// -------------------------------------------------------------- ranking

type rankEntry struct {
	AccountID string
	Name      string
	Ranked    bool
	Playlist  int
	Tier      int
	Division  int
	MMR       int
	Matches   int
	Streak    int
	MMRDelta  int
	HasDelta  bool
	PrevRank  int
	IsNew     bool
	Rank      int
}

// pickRepresentative chooses the playlist a member is ranked by: the
// first preferred playlist with matches played this season, else the
// highest-MMR active playlist. Returns -1 when nothing is active.
func pickRepresentative(byPlaylist map[int]rlapi.Skill) (int, bool) {
	for _, id := range repPreference {
		if s, ok := byPlaylist[id]; ok && s.MatchesPlayed > 0 {
			return id, true
		}
	}
	best := -1
	bestMMR := 0
	for id, s := range byPlaylist {
		if s.MatchesPlayed > 0 {
			if m := displayMMR(s); best == -1 || m > bestMMR {
				best, bestMMR = id, m
			}
		}
	}
	return best, best >= 0
}

func rankLine(e rankEntry) string {
	medal := ""
	switch e.Rank {
	case 1:
		medal = "🥇 "
	case 2:
		medal = "🥈 "
	case 3:
		medal = "🥉 "
	}
	pos := ""
	if e.PrevRank > 0 {
		if d := e.PrevRank - e.Rank; d > 0 {
			pos = fmt.Sprintf(" ⬆%d", d)
		} else if d < 0 {
			pos = fmt.Sprintf(" ⬇%d", -d)
		}
	}
	delta := ""
	if e.HasDelta {
		if e.MMRDelta > 0 {
			delta = fmt.Sprintf(" ▲+%d", e.MMRDelta)
		} else if e.MMRDelta < 0 {
			delta = fmt.Sprintf(" ▼%d", -e.MMRDelta)
		}
	}
	streak := ""
	if e.Streak >= 2 {
		streak = fmt.Sprintf(" 🔥%d連勝", e.Streak)
	}
	fresh := ""
	if e.IsNew {
		fresh = " 🆕"
	}
	short := playlistShort[e.Playlist]
	if short == "" {
		short = fmt.Sprintf("PL%d", e.Playlist)
	}
	return fmt.Sprintf("**%d.**%s %s**%s** — %s（%s） **%d**%s%s%s",
		e.Rank, pos, medal, e.Name, tierLabel(e.Tier, e.Division), short, e.MMR, delta, streak, fresh)
}

type boardMsg struct {
	Embed embed
	Hash  string
}

// buildBoards packs ranking lines into board messages. The header
// (update time) lives only in the first embed and is excluded from
// the content hash, so unchanged rankings do not trigger an edit and
// the shown time reads as "when the ranking last changed".
func buildBoards(ranked []rankEntry, unranked []rankEntry, memberCount int) []boardMsg {
	var lines []string
	for _, e := range ranked {
		lines = append(lines, rankLine(e))
	}
	if len(unranked) > 0 {
		lines = append(lines, "**―― 今季データなし ――**")
		for _, e := range unranked {
			lines = append(lines, fmt.Sprintf("・**%s**", e.Name))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "（表示できるメンバーがいません）")
	}

	const maxDesc = 3400 // runes, safely under Discord's 4096
	var groups [][]string
	var cur []string
	curLen := 0
	for _, ln := range lines {
		l := utf8.RuneCountInString(ln) + 1
		if len(cur) > 0 && curLen+l > maxDesc {
			groups = append(groups, cur)
			cur, curLen = nil, 0
		}
		cur = append(cur, ln)
		curLen += l
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}

	boards := make([]boardMsg, 0, len(groups))
	for i, g := range groups {
		h := sha256.Sum256([]byte(strings.Join(g, "\n")))
		emb := embed{Color: rankingColor}
		if i == 0 {
			emb.Title = "🏆 メンバーランキング"
			header := fmt.Sprintf("🕒 ランキング最終更新 <t:%d:R> ・ 対象 %d人\n", time.Now().Unix(), memberCount)
			emb.Description = header + strings.Join(g, "\n")
		} else {
			emb.Title = "🏆 メンバーランキング（続き）"
			emb.Description = strings.Join(g, "\n")
		}
		if i == len(groups)-1 {
			emb.Footer = &embedFooter{Text: "RL Rank Live • 代表モードは2v2優先で自動選択 • データ：Rocket League公式サーバー（PsyNet）"}
		}
		boards = append(boards, boardMsg{Embed: emb, Hash: hex.EncodeToString(h[:])})
	}
	return boards
}

// syncBoards posts/edits/deletes Discord messages to match boards.
func syncBoards(webhook string, st *boardState, boards []boardMsg) error {
	edited := 0
	for i, b := range boards {
		if i < len(st.BoardIDs) {
			if i < len(st.BoardHashes) && st.BoardHashes[i] == b.Hash {
				continue // unchanged: leave the message (and its time) alone
			}
			id, err := postOrEdit(webhook, st.BoardIDs[i], b.Embed)
			if err != nil {
				return err
			}
			st.BoardIDs[i] = id
			edited++
			continue
		}
		id, err := postOrEdit(webhook, "", b.Embed)
		if err != nil {
			return err
		}
		st.BoardIDs = append(st.BoardIDs, id)
		edited++
	}
	// Shrink: remove surplus boards from earlier, larger rankings.
	for len(st.BoardIDs) > len(boards) {
		last := st.BoardIDs[len(st.BoardIDs)-1]
		if err := deleteMessage(webhook, last); err != nil {
			return err
		}
		st.BoardIDs = st.BoardIDs[:len(st.BoardIDs)-1]
	}
	st.BoardHashes = make([]string, len(boards))
	for i, b := range boards {
		st.BoardHashes[i] = b.Hash
	}
	if edited == 0 {
		fmt.Println("Ranking unchanged: boards left untouched.")
	} else {
		fmt.Printf("Boards synced: %d message(s), %d written.\n", len(boards), edited)
	}
	// One-time cleanup of the v1 single-player board.
	if st.LegacyMessageID != "" {
		if err := deleteMessage(webhook, st.LegacyMessageID); err != nil {
			fmt.Printf("WARN: could not delete legacy board: %v\n", err)
		} else {
			fmt.Printf("Deleted legacy single-player board %s\n", st.LegacyMessageID)
			st.LegacyMessageID = ""
		}
	}
	return nil
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

func chunkIDs(ids []rlapi.PlayerID, n int) [][]rlapi.PlayerID {
	var out [][]rlapi.PlayerID
	for i := 0; i < len(ids); i += n {
		end := i + n
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[i:end])
	}
	return out
}

func cmdRun(post bool) error {
	targets, err := loadTargets()
	if err != nil {
		return err
	}

	auth, rpc, err := connect()
	if err != nil {
		return err
	}
	defer rpc.Close()
	fmt.Printf("Logged in as %s\n", auth.DisplayName)
	fmt.Printf("Targets: %d member(s)\n", len(targets))

	st := loadState()
	st.Version = 2

	// Resolve names -> account IDs (cached; only new names hit the
	// Epic lookup). A name that fails to resolve is skipped for this
	// run instead of failing the whole board.
	type member struct {
		targetName string
		accountID  string
		epicName   string
	}
	var members []member
	for _, name := range targets {
		if id, ok := st.Resolved[name]; ok && id != "" {
			members = append(members, member{targetName: name, accountID: id})
			continue
		}
		acc, err := lookupByDisplayName(auth.AccessToken, name)
		if err != nil {
			fmt.Printf("WARN: resolve %q failed, skipped this run: %v\n", name, err)
			continue
		}
		st.Resolved[name] = acc.ID
		members = append(members, member{targetName: name, accountID: acc.ID, epicName: acc.DisplayName})
		fmt.Printf("Resolved %q to a new account (cached)\n", name)
	}
	// Prune cached resolutions for names no longer targeted.
	{
		keep := map[string]bool{}
		for _, name := range targets {
			keep[name] = true
		}
		for name := range st.Resolved {
			if !keep[name] {
				delete(st.Resolved, name)
			}
		}
	}
	if len(members) == 0 {
		return fmt.Errorf("no members could be resolved")
	}

	ids := make([]rlapi.PlayerID, 0, len(members))
	for _, m := range members {
		ids = append(ids, rlapi.NewPlayerID(rlapi.PlatformEpic, m.accountID))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Batched skill fetch for everyone at once.
	skillsByPlayer := map[string][]rlapi.Skill{}
	for _, chunk := range chunkIDs(ids, 20) {
		players, err := rpc.GetPlayersSkills(ctx, chunk)
		if err != nil {
			return fmt.Errorf("GetPlayersSkills: %w", err)
		}
		for _, p := range players {
			skillsByPlayer[string(p.PlayerID)] = p.Skills
		}
	}
	// Current display names via profiles (rename-proof display).
	nameByPlayer := map[string]string{}
	for _, chunk := range chunkIDs(ids, 20) {
		profiles, err := rpc.GetProfiles(ctx, chunk)
		if err != nil {
			fmt.Printf("WARN: GetProfiles failed (names may be stale): %v\n", err)
			break
		}
		for _, p := range profiles {
			if p.PlayerName != "" {
				nameByPlayer[p.PlayerID] = p.PlayerName
			}
		}
	}

	var ranked, unranked []rankEntry
	for _, m := range members {
		pid := string(rlapi.NewPlayerID(rlapi.PlatformEpic, m.accountID))
		ms := st.Members[m.accountID]
		if ms == nil {
			ms = &memberState{}
			st.Members[m.accountID] = ms
		}
		if ms.Prev == nil {
			ms.Prev = map[string]skillSnapshot{}
		}
		// Display name priority: live profile > Epic lookup > last
		// known > the target list spelling.
		name := nameByPlayer[pid]
		if name == "" {
			name = m.epicName
		}
		if name == "" {
			name = ms.Name
		}
		if name == "" {
			name = m.targetName
		}
		ms.Name = name

		skills := skillsByPlayer[pid]
		byPlaylist := map[int]rlapi.Skill{}
		for _, s := range skills {
			byPlaylist[s.Playlist] = s
		}
		repID, active := pickRepresentative(byPlaylist)
		e := rankEntry{AccountID: m.accountID, Name: name, PrevRank: ms.PrevRank, IsNew: len(ms.Prev) == 0}
		if active {
			s := byPlaylist[repID]
			mmr := displayMMR(s)
			e.Ranked = true
			e.Playlist = repID
			e.Tier, e.Division = s.Tier, s.Division
			e.MMR, e.Matches, e.Streak = mmr, s.MatchesPlayed, s.WinStreak
			if prev, ok := ms.Prev[fmt.Sprintf("%d", repID)]; ok {
				e.HasDelta = true
				e.MMRDelta = mmr - prev.MMR
			}
			ranked = append(ranked, e)
		} else {
			unranked = append(unranked, e)
		}
		// Snapshot every returned playlist for future deltas.
		for _, s := range skills {
			key := fmt.Sprintf("%d", s.Playlist)
			ms.Prev[key] = skillSnapshot{Tier: s.Tier, Division: s.Division, MMR: displayMMR(s), Matches: s.MatchesPlayed, Streak: s.WinStreak}
		}
	}

	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.MMR != b.MMR {
			return a.MMR > b.MMR
		}
		if a.Tier != b.Tier {
			return a.Tier > b.Tier
		}
		if a.Division != b.Division {
			return a.Division > b.Division
		}
		if a.Matches != b.Matches {
			return a.Matches > b.Matches
		}
		return a.Name < b.Name
	})
	for i := range ranked {
		ranked[i].Rank = i + 1
		st.Members[ranked[i].AccountID].PrevRank = i + 1
	}
	sort.Slice(unranked, func(i, j int) bool { return unranked[i].Name < unranked[j].Name })
	for _, e := range unranked {
		st.Members[e.AccountID].PrevRank = 0
	}

	boards := buildBoards(ranked, unranked, len(members))

	if post {
		webhook := strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL"))
		if webhook == "" {
			return fmt.Errorf("DISCORD_WEBHOOK_URL is not set")
		}
		if err := syncBoards(webhook, &st, boards); err != nil {
			return err
		}
	} else {
		fmt.Println("Verification mode: ranking not posted. Rendered board(s):")
		for _, b := range boards {
			fmt.Printf("--- %s ---\n%s\n", b.Embed.Title, b.Embed.Description)
		}
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
