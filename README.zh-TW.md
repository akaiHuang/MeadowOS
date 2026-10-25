# MeadowOS

**讓 AI 和你的工作，待在同一個桌面。**

[English](README.md) · [開始使用](docs/QUICKSTART.md) · [連接 Inception](docs/INCEPTION.md) · [系統架構](docs/ARCHITECTURE.md)

MeadowOS 把聊天、檔案與日常工具放在一起。你可以在同一個 Linux 桌面與 Meadow 交談、
管理文件、開啟應用程式，讓 AI 協助處理工作。

![MeadowOS 桌面](docs/images/desktop.png)

## 一起工作的空間

- **熟悉的桌面。** 玻璃視窗、淺深色主題、精簡 Dock，以及可以新增與關閉的桌面分頁。
- **隨時能交談的 AI 同事。** 請 Meadow 協助處理檔案、使用工具與執行任務；背景活動另外收在工作紀錄。
- **一眼掌握檔案。** 依內容顯示的資料夾預覽、縮圖、空白鍵快速預覽與可調整的圖示大小。
- **留給聊天的空間。** 精簡聊天室支援 Markdown、附件與浮動輸入框。
- **VoiceOS 預覽。** 實驗性的全螢幕視覺介面；此 Alpha 版尚未提供語音輸入與播放。

![MeadowOS 聊天室](docs/images/chat.png)

## 開始使用

先安裝 **Docker（含 Compose）** 與 **Git**，再執行：

```sh
git clone https://github.com/akaiHuang/MeadowOS.git
cd MeadowOS
docker compose build --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)"
docker compose up -d
```

開啟 [MeadowOS 桌面](http://127.0.0.1:60511/vnc.html?autoconnect=true&resize=scale)。
首次建置需要幾分鐘，請先確認 60511、60531 連接埠未被使用。

[完整啟動說明 →](docs/QUICKSTART.md)

## 連接 Meadow

Meadow 使用 **Inception Mercury 3**。先到 [Inception](https://platform.inceptionlabs.ai/) 註冊，
再從 [API Keys](https://platform.inceptionlabs.ai/dashboard/api-keys) 建立 API key。

啟動 MeadowOS 後，在專案目錄執行：

```sh
docker compose exec -it meadowos python3 /usr/local/share/meadow-os/configure_inception.py
```

在隱藏提示貼上金鑰，接著打開聊天室的 **⋮ → 重新連線**，就可以開始交談。
API 費用由 Inception 依使用量計算。

[API 設定教學與常見問題 →](docs/INCEPTION.md)

## Alpha 版支援範圍

目前透過 Docker 執行 Ubuntu 26.04／X11 桌面，支援 Linux `amd64` 與 `arm64`；
macOS、Windows 可使用 Docker Desktop。

Mercury 3 支援文字與工具，目前無法辨識圖片。部分 Docker 主機會限制 WebKit 沙箱，
導致 GNOME Web 無法啟動。此版本尚未提供可開機安裝器與 Wayland 支援。

![MeadowOS 設定](docs/images/settings.png)

## 了解專案

[系統架構](docs/ARCHITECTURE.md) · [開發與測試](docs/VERIFICATION.md) ·
[回報問題](https://github.com/akaiHuang/MeadowOS/issues)

採用 [Apache-2.0](LICENSE) 授權。第三方元件請見 [NOTICE](NOTICE) 與 [授權清單](docs/THIRD_PARTY.md)。
