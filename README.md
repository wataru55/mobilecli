# mobilecli (fork)

[mobile-next/mobilecli](https://github.com/mobile-next/mobilecli) のforkです。基本機能・導入方法は[fork元のREADME](https://github.com/mobile-next/mobilecli#readme)を参照してください。

## forkで追加した機能・改善とその理由

- **UIツリーの取得元を選択**：用途に応じて取得内容と処理時間を選べるよう、`dump ui` / `snapshot` に `--source render|semantics|ax` を追加しました。Flutterの描画ツリー、Semanticsツリー、OSのアクセシビリティツリーを切り替えられます。
- **FlutterのUI取得を高速化**：描画ツリーをたどる方式では大量のRPCが必要で、画面によって取得に数十秒かかっていました。`--source semantics` ではラベル・識別子・座標などを1回のRPCで取得します。Dart VM serviceに接続でき、Semanticsが有効なアプリが対象です。
- **取得元に合わせた要素操作**：`@e5` などの要素参照は、取得元が変わると別の要素を指します。誤操作を防ぐため、`io tap` / `io longpress` にも `--source` を追加し、UI取得時と同じツリーを指定できるようにしました。
- **iOSシミュレータの誤接続を修正**：複数のシミュレータで同じアプリを動かすと、指定した端末とは別のUIが返ることがありました。対象端末のアプリプロセスからDart VM serviceの接続先を特定するようにしました。
- **Androidの不要な処理を削減**：FlutterのUI取得に必要なのはフォアグラウンドのパッケージ名だけですが、アプリ名の解決のために全アプリを取得していました。この余分な待ち時間を減らすため、アプリ一覧の取得を省きました。
- **調査ツールを追加**：UI取得の遅さを調べ、高速な取得方法を探すために `devtools/vmeval` を追加しました。Dart VM serviceに任意のRPCを送り、応答内容・所要時間・利用可能な拡張機能を確認できます。
