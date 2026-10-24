# Inception API 設定教學

準備好一組自己的 Inception API key，約幾分鐘就能在 MeadowOS 選用 Mercury 3。MeadowOS 不需要 Meadow 帳號；API 用量由你的 Inception 帳戶計費。

## 1. 申請 Inception API key

1. 前往 [Inception 平台](https://platform.inceptionlabs.ai/)，註冊或登入你的帳號。
2. 開啟 [API Keys 頁面](https://platform.inceptionlabs.ai/dashboard/api-keys)，建立一組新的 API key。若平台要求啟用帳務，依平台畫面完成。
3. 複製完整金鑰。下一步會提供隱藏輸入提示；請在該提示中貼上。

官方說明：[快速開始](https://docs.inceptionlabs.ai/get-started)、[模型清單](https://docs.inceptionlabs.ai/get-started/models)。

## 2. 在已啟動的 MeadowOS 填入金鑰

在電腦的終端機進入這份專案目錄。先依 [README](../README.md) 啟動 MeadowOS，再執行：

```sh
docker compose exec -it meadowos python3 /usr/local/share/meadow-os/configure_inception.py
```

看到以下提示時，貼上金鑰並按 Enter：

```text
貼上 Inception API key（輸入不會顯示）：
```

輸入時沒有星號、也沒有文字是正常的。程式會讀取 Inception 的模型清單，確認你的帳戶能看到 `mercury-3`，再透過正在執行的 Meadow Agent 儲存設定。

成功時會顯示：

```text
已驗證 Inception 模型清單，並儲存、套用 Mercury 3（工具已啟用）。
```

回到 [MeadowOS 桌面](http://127.0.0.1:60511/vnc.html?autoconnect=1&resize=scale)，在聊天室點右上角的聊天選單（⋮），選擇「重新連線」。第一次設定前尚未送出訊息的空白聊天室，會接上 Mercury 3，接著就能輸入訊息。不需要重建或重啟容器。

若聊天室已經有內容，例如之後更換金鑰，原對話會保留原來的模型連線。請從桌面左上角的「應用程式」開啟「Meadow Code」，再點左側「新聊天」，使用剛設定的連線。舊對話與工作檔案會保留。

你可以先輸入「請在工作區建立 hello.txt，內容是 Hello from MeadowOS」，確認首次模型回答與工具操作。模型清單驗證與設定完成後，實際回答仍取決於當時的 Inception 服務和帳戶額度。

## 3. 保存與之後更換金鑰

金鑰會由原生 Agent 以獨立的私人檔案儲存在 Docker 的 `meadow-home` 持久資料卷中。金鑰檔案權限為 `0600`，憑證目錄為 `0700`；模型設定檔只有連線資訊，不含金鑰。

重新執行相同指令就能更換金鑰。若有工作正在執行，Agent 會拒絕設定切換；等工作完成後再執行。設定程式不會停止工作。

日常停止、啟動與重建容器會保留這個資料卷。`docker compose down -v` 會刪除資料卷，其中包含金鑰、對話與工作檔案；需要保留資料時請使用不帶 `-v` 的指令。

請把金鑰只貼在隱藏提示中。設定程式不接受 `--api-key`、環境變數或管線輸入，因此不需要把金鑰寫進 `.env`、Compose 檔、Dockerfile、指令列或 Git。

## 常見狀況

| 畫面 | 處理方式 |
| --- | --- |
| 需要互動式終端機 | 使用完整的 `docker compose exec -it ...` 指令，直接在終端機執行。 |
| 無法驗證正在執行的 Meadow Agent | 先依 README 啟動 MeadowOS，等待容器完成啟動再試。 |
| Inception 未授權此金鑰 | 回 API Keys 頁面確認金鑰、帳戶權限與帳務狀態，再重新執行。 |
| 此帳戶未列出 Mercury 3 | 到 Inception 平台確認此帳戶目前能使用 `mercury-3`。 |
| 無法取得閒置設定鎖 | 等 Agent 的工作完成後再設定。 |
| 程式中斷或尚未確認設定套用 | 重新執行指令。原生閒置鎖最長在 15 秒後自動到期。 |

設定程式固定使用 `https://api.inceptionlabs.ai/v1`、OpenAI 相容協定和模型 `mercury-3`。Mercury 3 的工具能力會明確啟用，圖片能力關閉，推理控制使用 `inception-effort`。模型清單的檢查不會發送聊天請求。

## English quick start

Create your own key in the [Inception API Keys dashboard](https://platform.inceptionlabs.ai/dashboard/api-keys). With MeadowOS running, execute this command from the project directory:

```sh
docker compose exec -it meadowos python3 /usr/local/share/meadow-os/configure_inception.py
```

Paste the key only at the hidden prompt. The helper checks the official model listing, uses the authenticated local Agent API, saves private credentials in the persistent home volume, and selects Mercury 3 with tools enabled. In the desktop chat, open its three-dot menu and select “重新連線” (Reconnect). A first, unsubmitted empty chat then uses the new connection. For an existing conversation after key rotation, open “Meadow Code” from Applications and click “新聊天” (New chat) in its sidebar; existing chats retain their original binding. Busy Agents reject configuration changes until work finishes. Re-run the command to rotate your key; no image rebuild is needed. Keep the `meadow-home` volume to retain settings and work.
