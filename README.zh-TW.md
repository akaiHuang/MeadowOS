# MeadowOS

**把原生 Linux 桌面與持續工作的 AI 同事放在同一個工作空間。**

[English](README.md) · [完整啟動說明](docs/QUICKSTART.md) · [Inception API 教學](docs/INCEPTION.md) · [架構](docs/ARCHITECTURE.md)

MeadowOS 提供玻璃風格桌面、真正的 Linux 應用程式、檔案預覽、持久化聊天室與背景 Agent。
AI 使用你自己申請的 **Inception Mercury 3 API key**；不內附任何人的金鑰，也不需要 Meadow 帳號。

目前是早期開發版，透過 Docker 執行 Ubuntu 26.04／X11 原生桌面，尚未提供可開機 ISO 或完整 VM 安裝器。

![MeadowOS 原生桌面](docs/images/desktop.png)

## 開始使用

安裝 Docker 與 Compose 後，在終端機執行：

```sh
git clone https://github.com/akaiHuang/MeadowOS.git
cd MeadowOS
docker compose build --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)"
docker compose up -d
```

開啟 [MeadowOS 桌面](http://127.0.0.1:60511/vnc.html?autoconnect=true&resize=scale)。
第一次會下載套件並編譯原生程式。60511、60531 連接埠需要保持空閒。
尚未設定金鑰也能先操作桌面。

## 申請與填入 Inception API

1. 前往 [Inception 平台](https://platform.inceptionlabs.ai/) 註冊或登入。
2. 在 [API Keys](https://platform.inceptionlabs.ai/dashboard/api-keys) 建立你自己的金鑰。
3. 回到專案目錄執行：

   ```sh
   docker compose exec -it meadowos python3 /usr/local/share/meadow-os/configure_inception.py
   ```

4. 在隱藏輸入提示貼上金鑰並按 Enter。看不到文字或星號是正常的。
5. 成功後，依程式提示回聊天室重新連線，再開始交談。

[查看完整設定步驟與常見問題](docs/INCEPTION.md)。程式會驗證官方模型清單，啟用 Mercury 3 的工具能力；
API 費用與可用額度以 Inception 帳戶顯示為準。

## 可以做什麼

- 玻璃桌面、統一視窗控制、淺／深色主題與 Dock。
- 桌面分頁：＋新增，移到分頁時出現 ×；關閉桌面時，視窗會移到相鄰桌面。
- 原生檔案管理、依內容生成的資料夾預覽、圖示大小調整與空白鍵預覽。
- 浮動輸入框聊天室、Markdown、背景工作紀錄與持久化對話。
- Agent 的檔案、終端機、桌面、瀏覽器與排程工具。
- 實驗性的 VoiceOS 全螢幕視覺介面；Linux 麥克風、辨識與語音播放仍需要相容的語音後端。

Mercury 3 是文字／工具模型。加入圖片時會保留縮圖與附件，並明確提示目前無法辨識圖片。

## 金鑰與資料放在哪裡

金鑰由原生憑證儲存層放在你的 Docker `meadow-home` 資料卷中，與模型目錄分開保存；
不會放進程式碼、Docker 映像、`.env` 或指令參數。聊天室、工作檔案也保存在同一個私人 home 資料卷。

預設只開放本機連接埠，以非 root 使用者執行，且不掛載主機家目錄或 Docker socket。
Agent 具有容器內桌面、檔案與指令的完整操作能力；自行新增主機掛載前，請先確認你要提供的範圍。

停止並保留資料：`docker compose stop`。再次啟動：`docker compose up -d`。
`docker compose down` 會保留資料卷；加上 `-v` 則會刪除其中的金鑰、對話與檔案。

## 架構與驗證

[架構文件](docs/ARCHITECTURE.md) 說明 GTK Shell、單一持久 Agent、模型、工具、排程與資料的關係。
模型負責判斷任務；harness 負責工具、狀態、權限與真實結果，不把工具呼叫成功當成任務完成。

CI 會從乾淨來源建置，使用沒有金鑰的新資料卷驗證桌面、檔案、終端機、設定與重啟保存；API 設定整合使用本機模擬服務測試，沒有呼叫真實帳戶。
這與真實模型任務、VM 開機驗收是不同層次。

容器限制：GNOME Web 與選用的 Wails Code Desktop 使用 WebKit。部分主機禁止容器建立巢狀命名空間，會讓 WebKit 沙箱無法啟動。
CI 會另行記錄這兩個程式的可用性；預設不關閉沙箱或提高容器權限。原生 GTK 聊天室與 API 設定工具不依賴 Wails，
金鑰更新後可由聊天選單「新對話」開始使用。[查看啟動與疑難排解](docs/QUICKSTART.md)。
目前不宣稱 Wayland、可開機安裝器、GPU 效能保證或完整 Linux 語音體驗。

![MeadowOS 系統設定](docs/images/settings.png)

原始碼採 [Apache-2.0](LICENSE)，第三方元件保留各自授權，見 [NOTICE](NOTICE) 與 [第三方清單](docs/THIRD_PARTY.md)。
