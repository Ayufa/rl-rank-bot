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
4. **ランキング化と描画**: 主要3モード（**3v3 → 2v2 → 1v1 の順に投稿**）
   ごとに、そのモードで今季試合のあるメンバーだけを MMR 降順に並べ、
   **画像ボード**を生成する。表示は **順位・名前・ランク・レート
   （MMR）・過去7日のレート変化**。ランク名は略記（Grand Champion→GC、
   Champion→C、Diamond→D、Platinum→P、Gold→G、Silver→S、Bronze→B、
   Supersonic Legend→SSL。Div 表記はそのまま）。レートの隣に**本物の
   ランクアイコン**（Tier ごとに23種、Rocket League Wiki 素材）を
   表示する。アイコンはゲーム内同様 Tier 単位で異なり、同一 Tier 内の
   Division I–IV は同じアイコン（Div は文字で表示）。右上の赤ドット
   **LIVE バッジ**でリアルタイム集計中であることを示す
   - **週次デルタ**: 実行ごとにレートを state の履歴へ記録（30分に
     1回へ間引き）し、「今 − その週の最初の記録」をレートの隣に
     ▲+n（緑）/ ▼-n（赤）/ ±0（グレー）で表示する。週の起点は
     **月曜 00:00（JST）** で、週が替わると基準を取り直して
     リセットされる。初日から ±0 で表示され、日ごとに積み重なって
     いく様子が見える。アイコン・レート・デルタは固定スロット
     配置で、値の桁数や有無によらず縦に揃う
   - デザインはショップBotと同系（濃紺ボード、Michroma タイトル、
     Noto Sans JP）。**1モード＝1枚**が原則で、人数が増えたら画像が
     縦に伸びる（行サイズとフォントは変えないので見やすさは不変）。
     そのためボード同士の高さは揃わない
5. **Discord**: 画像のみを投稿し、以降は**同じメッセージを編集して更新**
   する（Webhook の multipart 投稿／PATCH、ショップBotで実績のある方式）。
   毎回すべてのボードを編集し直すため、画像内の時刻は常に最新の
   確認時刻になる。チャンネルの並び順は投稿順で
   決まり編集では変えられないため、並び自体を変えるときだけ
   ワークフローの `reset` 入力で全削除→再投稿し、以後は編集のみに戻る

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
オン = ボード投稿／編集。`reset` オン（post と併用）= 既存ボードを
全削除して現在のモード順で再投稿する（並び替え時の一回限り用）。

旧形式（テキスト版）のボードは、初回の画像ボード実行時に置き換え／削除
される（state の移行処理）。

## トークン復旧手順

refresh token の所有者は Actions cache（`state/`）。cache は7日使われないと
消えるため、長期間実行しないとチェーンが切れることがある。その場合は:

1. `rlrank auth-url` → ログイン → `rlrank auth-code <code>` で再ログイン
2. Secret `RL_RANK_REFRESH_TOKEN` を新しい token.txt の中身で更新
3. Actions cache を削除してから dispatch（空なら Secret から再シードされる）

ローカルの token.txt はシード用の一時コピーで、Actions が回り始めたら無効。
ローカルで `run` してはいけない（チェーンを壊す）。
