# RL Rank Live Board

Discord の投稿を編集し続けて、対象メンバー全員の Rocket League ランクを
**ランキング形式**でライブ表示する Bot。投稿は Webhook、取得は全部
プログラム（手作業なし）。

## 仕組み

1. **専用 EGS ログイン**（この Bot 専用。`rl-shop-bot` とは完全に別チェーン）
   - 初回だけ `rlrank auth-url` の URL で Epic にログインし、認証コードを
     `rlrank auth-code <code>` に渡す → refresh token を保存
   - refresh token は使うたびにローテーションするので、所有者は1か所だけ。
     本番では GitHub Actions の cache（`state/`）が所有し、ローカルの
     token.txt はシード後は無効になる（ローカルで `run` しないこと）
2. **名前 → アカウントID 変換**: Epic アカウントサービスの displayName 照会
   （ユーザートークン必須。アプリ用トークンでは 403 になることを確認済み）。
   変換結果は state にキャッシュし、以降は ID で追うので、相手が表示名を
   変更しても追跡が切れない（表示名は毎回プロフィールから最新化）
3. **ランク取得**: PsyNet（dank/rlapi）の `GetPlayersSkills` で全員分を
   一括取得（20人ずつ分割）。プレイリストごとに Tier / Division / MMR /
   連勝 / 試合数
4. **ランキング化**: 各メンバーの「代表モード」（今季試合数のあるモードを
   2v2 → 3v3 → 1v1 → その他MMR最大の順で自動選択）で MMR 降順に並べ、
   順位・前回順位との差（⬆⬇）・MMR前回比（▲▼）・連勝🔥を1行ずつ表示。
   今季データのないメンバーは末尾に分けて載せる
5. **Discord**: ボードを投稿し、以降は同じメッセージを編集。
   内容が変わっていないボードは編集しない（表示時刻＝ランキングが
   最後に変わった時刻になる）。メンバーが増えて1通に収まらない場合は
   自動で複数通に分割し、減ったら余分な通を削除する

## 対象メンバー

- `targets.txt` に Epic 表示名を1行に1つ。追加・削除はファイル編集＋push
- 全プラットフォーム共通で Epic 表示名で指定する（F2P 以降、全プレイヤーが
  Epic アカウントを持つため。Switch の人も Epic 名で引ける）
- 名前は特殊文字まで完全一致が必要（例: `Αyufa` の先頭はギリシャ文字
  大文字 Alpha U+0391）
- `targets.txt` が無い環境では `RL_TARGET_NAME`（単独指定）にフォールバック

## Secrets

- `RL_RANK_REFRESH_TOKEN` — 専用 EGS ログインの refresh token（初回シード用）
- `DISCORD_WEBHOOK_URL` — ランクボード用チャンネルの Webhook

## 実行

GitHub Actions の `RL Rank Live Board` ワークフローを手動 dispatch。
`post` オフ = 検証のみ（生成されるランキングをログに出力）、
オン = ボード投稿／編集。

初回のランキング実行時に、旧・1人用詳細ボード（v1 の投稿）は自動で
削除される（state の移行処理）。

## トークン復旧手順

refresh token の所有者は Actions cache（`state/`）。cache は7日使われないと
消えるため、長期間実行しないとチェーンが切れることがある。その場合は:

1. `rlrank auth-url` → ログイン → `rlrank auth-code <code>` で再ログイン
2. Secret `RL_RANK_REFRESH_TOKEN` を新しい token.txt の中身で更新
3. Actions cache を削除してから dispatch（空なら Secret から再シードされる）

ローカルの token.txt はシード用の一時コピーで、Actions が回り始めたら無効。
ローカルで `run` してはいけない（チェーンを壊す）。
