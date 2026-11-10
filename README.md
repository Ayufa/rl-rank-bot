# RL Rank Live Board

Discord の1投稿を編集し続けて、特定プレイヤーの Rocket League ランク／MMR を
ライブ表示する Bot。投稿は Webhook、取得は全部プログラム（手作業なし）。

## 仕組み

1. **専用 EGS ログイン**（この Bot 専用。`rl-shop-bot` とは完全に別チェーン）
   - 初回だけ `rlrank auth-url` の URL で Epic にログインし、認証コードを
     `rlrank auth-code <code>` に渡す → refresh token を保存
   - refresh token は使うたびにローテーションするので、所有者は1か所だけ。
     本番では GitHub Actions の cache（`state/`）が所有し、ローカルの
     token.txt はシード後は無効になる（ローカルで `run` しないこと）
2. **名前 → アカウントID 変換**: Epic アカウントサービスの displayName 照会
   （ユーザートークン必須。アプリ用トークンでは 403 になることを確認済み）
3. **ランク取得**: PsyNet（dank/rlapi）の `GetPlayersSkills` で
   プレイリストごとに Tier / Division / MMR / 連勝 / 試合数
4. **Discord**: ボードを1件投稿し、以降は同じメッセージを編集。
   前回値との差分（▲▼）も表示する

## 対象

- Epic 表示名 `Αyufa`（先頭はギリシャ文字の大文字 Alpha U+0391）
- 環境変数 `RL_TARGET_NAME` で変更可

## Secrets

- `RL_RANK_REFRESH_TOKEN` — 専用 EGS ログインの refresh token（初回シード用）
- `DISCORD_WEBHOOK_URL` — ランクボード用チャンネルの Webhook

## 実行

GitHub Actions の `RL Rank Live Board` ワークフローを手動 dispatch。
`post` オフ = 検証のみ（スキルをログに出力）、オン = ボード投稿／編集。
