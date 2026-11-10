// rlrank: Rocket League member ranking boards for Discord.
//
// Fully programmatic pipeline (no manual fetching):
//  1. EGS refresh-token login (a DEDICATED login for this bot only,
//     created once via `auth-code`; never shared with rl-shop-bot).
//  2. Epic account-service displayName lookup: member name -> account
//     ID (cached in state; later runs resolve by ID, so renames do
//     not break tracking).
//  3. PsyNet (via dank/rlapi) GetPlayersSkills for ALL members in
//     batched calls: per-playlist Tier / Division / MMR / Matches.
//  4. For each main playlist (2v2 / 3v3 / 1v1) the members with games
//     this season are ranked by MMR and rendered as a designed image
//     board (board.go) showing only position, name, rank and rating.
//     The board messages are EDITED in place on later runs; boards
//     whose ranking did not change are left untouched.
//
// Usage:
//
//	rlrank auth-url            print the Epic login URL for `auth-code`
//	rlrank auth-code <code>    exchange an authorization code, save token
//	rlrank run [--post]        fetch once; --post also updates Discord
//	rlrank demo                render sample boards to ./out (no login)
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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

// mainModes are the playlists that get their own ranking board, in
// posting order (channel order follows post order: 3v3, 2v2, 1v1).
var mainModes = []struct {
	id    int
	label string
}{
	{13, "3v3"},
	{11, "2v2"},
	{10, "1v1"},
}

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

type boardState struct {
	Version     int               `json:"version,omitempty"`
	BoardIDs    []string          `json:"board_ids,omitempty"`
	BoardHashes []string          `json:"board_hashes,omitempty"`
	Resolved    map[string]string `json:"resolved,omitempty"` // target name -> account ID
	UpdatedAt   string            `json:"updated_at,omitempty"`

	// Legacy v1 field (single-player text board), deleted once.
	LegacyMessageID string `json:"message_id,omitempty"`
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

// sendBoardImage posts (messageID == "") or edits one image board
// message and returns its message ID. The image is the whole post
// (image-only, like the shop bot). The multipart shape — payload_json
// with attachments[0] bound to files[0] — is the one proven by
// rl-shop-bot, including PATCH edits that swap the image.
func sendBoardImage(webhook, messageID string, b imgBoard) (string, error) {
	fields := map[string]any{
		"attachments": []map[string]any{{"id": 0, "filename": b.Filename}},
	}
	method, suffix := "POST", "?wait=true"
	if messageID != "" {
		method, suffix = "PATCH", "/messages/"+messageID+"?wait=true"
	} else {
		fields["username"] = "RL Rank Live"
	}
	payload, _ := json.Marshal(fields)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("payload_json", string(payload))
	fw, err := mw.CreateFormFile("files[0]", b.Filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(b.Bytes); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequest(method, webhook+suffix, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if method == "PATCH" && resp.StatusCode == http.StatusNotFound {
		// Message was deleted in Discord: repost as a new board.
		return sendBoardImage(webhook, "", b)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("discord %s board: HTTP %d: %s", method, resp.StatusCode, string(out))
	}
	if messageID != "" {
		return messageID, nil
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
	req, err := http.NewRequest("DELETE", webhook+"/messages/"+messageID, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("delete message %s: HTTP %d: %s", messageID, resp.StatusCode, string(out))
}

// syncBoards posts/edits/deletes Discord messages to match boards.
func syncBoards(webhook string, st *boardState, boards []imgBoard) error {
	written := 0
	for i, b := range boards {
		if i < len(st.BoardIDs) {
			if i < len(st.BoardHashes) && st.BoardHashes[i] == b.Hash {
				continue // unchanged ranking: leave message (and its time) alone
			}
			id, err := sendBoardImage(webhook, st.BoardIDs[i], b)
			if err != nil {
				return err
			}
			st.BoardIDs[i] = id
			written++
			continue
		}
		id, err := sendBoardImage(webhook, "", b)
		if err != nil {
			return err
		}
		st.BoardIDs = append(st.BoardIDs, id)
		written++
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
	if written == 0 {
		fmt.Println("Rankings unchanged: boards left untouched.")
	} else {
		fmt.Printf("Boards synced: %d message(s), %d written.\n", len(boards), written)
	}
	// One-time cleanup of any pre-image (v1/v2 text) board.
	if st.LegacyMessageID != "" {
		if err := deleteMessage(webhook, st.LegacyMessageID); err != nil {
			fmt.Printf("WARN: could not delete legacy board: %v\n", err)
		} else {
			fmt.Printf("Deleted legacy board %s\n", st.LegacyMessageID)
			st.LegacyMessageID = ""
		}
	}
	return nil
}

// -------------------------------------------------------------- ranking

// buildImageBoards renders every mode's ranking, splitting a mode
// into several boards when it has more than rowsPerBoard members.
// The ordering is deterministic (2v2, 3v3, 1v1, then part 2, ...)
// so positional state (message IDs / hashes) lines up across runs.
func buildImageBoards(perMode map[int][]boardRow, updated time.Time) ([]imgBoard, error) {
	var boards []imgBoard
	for _, mode := range mainModes {
		rows := perMode[mode.id]
		chunks := [][]boardRow{rows}
		if len(rows) > rowsPerBoard {
			chunks = nil
			for i := 0; i < len(rows); i += rowsPerBoard {
				end := i + rowsPerBoard
				if end > len(rows) {
					end = len(rows)
				}
				chunks = append(chunks, rows[i:end])
			}
		}
		for ci, chunk := range chunks {
			label := mode.label
			filename := fmt.Sprintf("ranking-%s.jpg", strings.ToLower(mode.label))
			if len(chunks) > 1 {
				label = fmt.Sprintf("%s (%d/%d)", mode.label, ci+1, len(chunks))
				filename = fmt.Sprintf("ranking-%s-%d.jpg", strings.ToLower(mode.label), ci+1)
			}
			img, err := renderBoard(label, chunk, ci*rowsPerBoard+1, updated)
			if err != nil {
				return nil, err
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%s\n", mode.label)
			base := ci * rowsPerBoard
			for ri, r := range chunk {
				fmt.Fprintf(&sb, "%d|%s|%d|%d|%d\n", base+ri+1, r.Name, r.Tier, r.Division, r.MMR)
			}
			sum := sha256.Sum256([]byte(sb.String()))
			boards = append(boards, imgBoard{Filename: filename, Bytes: img, Hash: hex.EncodeToString(sum[:])})
		}
	}
	return boards, nil
}

func sortRows(rows []boardRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
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

func cmdRun(post, reset bool) error {
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
	st.Version = 3

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

	perMode := map[int][]boardRow{}
	for _, m := range members {
		pid := string(rlapi.NewPlayerID(rlapi.PlatformEpic, m.accountID))
		name := nameByPlayer[pid]
		if name == "" {
			name = m.epicName
		}
		if name == "" {
			name = m.targetName
		}
		byPlaylist := map[int]rlapi.Skill{}
		for _, s := range skillsByPlayer[pid] {
			byPlaylist[s.Playlist] = s
		}
		for _, mode := range mainModes {
			s, ok := byPlaylist[mode.id]
			if !ok || s.MatchesPlayed == 0 {
				continue // not active in this mode this season
			}
			perMode[mode.id] = append(perMode[mode.id], boardRow{
				Name: name, Tier: s.Tier, Division: s.Division,
				MMR: displayMMR(s), Matches: s.MatchesPlayed,
			})
		}
	}
	for _, mode := range mainModes {
		sortRows(perMode[mode.id])
	}

	boards, err := buildImageBoards(perMode, time.Now())
	if err != nil {
		return fmt.Errorf("render boards: %w", err)
	}

	if post {
		webhook := strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL"))
		if webhook == "" {
			return fmt.Errorf("DISCORD_WEBHOOK_URL is not set")
		}
		if reset && len(st.BoardIDs) > 0 {
			// One-time reorder/repair: Discord message order in a
			// channel follows post time and cannot be changed by
			// editing, so the old boards are deleted and reposted
			// in the current mode order. Later runs edit in place.
			n := 0
			for _, id := range st.BoardIDs {
				if err := deleteMessage(webhook, id); err != nil {
					fmt.Printf("WARN: reset delete %s: %v\n", id, err)
				} else {
					n++
				}
			}
			fmt.Printf("Reset: deleted %d old board(s); reposting fresh.\n", n)
			st.BoardIDs, st.BoardHashes = nil, nil
		}
		if err := syncBoards(webhook, &st, boards); err != nil {
			return err
		}
	} else {
		fmt.Println("Verification mode: boards not posted. Rankings:")
		for _, mode := range mainModes {
			rows := perMode[mode.id]
			fmt.Printf("[%s] %d player(s)\n", mode.label, len(rows))
			for i, r := range rows {
				fmt.Printf("  #%d %s — %s — %d\n", i+1, r.Name, tierLabel(r.Tier, r.Division), r.MMR)
			}
		}
	}

	st.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveState(st)
}

// cmdDemo renders sample boards locally (no login, no network) so
// the design can be checked before anything is posted.
func cmdDemo() error {
	perMode := map[int][]boardRow{
		11: {
			{Name: "RocketTaro", Tier: 22, Division: 0, MMR: 1902, Matches: 210},
			{Name: "Αyufa", Tier: 19, Division: 2, MMR: 1524, Matches: 89},
			{Name: "さくら", Tier: 17, Division: 0, MMR: 1411, Matches: 64},
			{Name: "zeekend_14", Tier: 15, Division: 1, MMR: 1003, Matches: 41},
			{Name: "xX_ShadowSniper_99_Xx", Tier: 10, Division: 3, MMR: 768, Matches: 33},
			{Name: "mochi", Tier: 7, Division: 1, MMR: 512, Matches: 12},
			{Name: "PlacementPro", Tier: 0, Division: 0, MMR: 498, Matches: 6},
			{Name: "B2", Tier: 2, Division: 0, MMR: 214, Matches: 18},
		},
		13: {
			{Name: "Αyufa", Tier: 18, Division: 3, MMR: 1403, Matches: 23},
			{Name: "さくら", Tier: 16, Division: 2, MMR: 1250, Matches: 51},
			{Name: "zeekend_14", Tier: 13, Division: 0, MMR: 940, Matches: 29},
		},
		10: {},
	}
	for _, mode := range mainModes {
		sortRows(perMode[mode.id])
	}
	boards, err := buildImageBoards(perMode, time.Now())
	if err != nil {
		return err
	}
	if err := os.MkdirAll("out", 0o755); err != nil {
		return err
	}
	for _, b := range boards {
		p := filepath.Join("out", "demo-"+b.Filename)
		if err := os.WriteFile(p, b.Bytes, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", p)
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rlrank <auth-url|auth-code|run|demo> [--post]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "auth-url":
		err = cmdAuthURL()
	case "auth-code":
		err = cmdAuthCode(os.Args[2:])
	case "demo":
		err = cmdDemo()
	case "run":
		post, reset := false, false
		for _, a := range os.Args[2:] {
			if a == "--post" {
				post = true
			}
			if a == "--reset" {
				reset = true
			}
		}
		err = cmdRun(post, reset)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
