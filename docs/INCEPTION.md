# 連接 Inception Mercury 3

使用自己的 Inception API key，即可在 MeadowOS 與 Meadow 交談、處理檔案和執行任務。API 用量由 Inception 計費，不需要 Meadow 帳號。

## 1. 申請 API key

1. 前往 [Inception 平台](https://platform.inceptionlabs.ai/) 註冊或登入。
2. 在 [API Keys](https://platform.inceptionlabs.ai/dashboard/api-keys) 建立一組金鑰。
3. 若平台要求啟用帳務，依畫面完成後複製金鑰。

## 2. 填入金鑰

先依 [啟動教學](QUICKSTART.md) 開啟 MeadowOS，再從電腦的終端機進入專案目錄，執行：

```sh
docker compose exec -it meadowos python3 /usr/local/share/meadow-os/configure_inception.py
```

在提示出現後貼上金鑰，按 Enter。輸入不會顯示文字或星號，這是正常的。

看到「已驗證 Inception 模型清單，並儲存、套用 Mercury 3」即完成設定。

## 3. 開始交談

回到 [MeadowOS 桌面](http://127.0.0.1:60511/vnc.html?autoconnect=true&resize=scale)，開啟聊天室右上角的選單 **⋮**：

- 第一次設定，且聊天室尚未送出過訊息：選擇 **重新連線**。
- 已有對話，或之後更換金鑰：選擇 **新對話**。

不需要重新建置或重啟 MeadowOS。可以先試著說：

> 請在工作區建立 hello.txt，內容是 Hello from MeadowOS。

之後需要更換金鑰時，再執行一次相同的設定指令即可。

## 遇到問題

| 狀況 | 處理方式 |
| --- | --- |
| 提示需要互動式終端機 | 在終端機執行完整指令，保留 `-it`。 |
| 無法連接 MeadowOS | 確認 MeadowOS 已啟動，稍等片刻再試。 |
| 金鑰未授權，或無法使用 Mercury 3 | 到 Inception 平台確認金鑰、模型存取權限與帳務狀態。 |
| 目前有任務執行中 | 等任務完成後，再重新設定。 |

Mercury 3 支援文字與工具操作，目前這個連線不提供圖片辨識。

更多資訊：[Inception 快速開始](https://docs.inceptionlabs.ai/get-started) · [模型清單](https://docs.inceptionlabs.ai/get-started/models)

## English quick start

1. Create your own key in the [Inception API Keys dashboard](https://platform.inceptionlabs.ai/dashboard/api-keys).
2. With MeadowOS running, open a terminal in the project directory and run:

   ```sh
   docker compose exec -it meadowos python3 /usr/local/share/meadow-os/configure_inception.py
   ```

3. Paste your key at the hidden prompt and press Enter. No characters will appear while you type.
4. In the desktop chat, open **⋮ → 重新連線 (Reconnect)** for a first, empty conversation. If you already have a conversation or are changing keys, choose **新對話 (New conversation)** instead.

No rebuild or restart is needed. API usage is billed by Inception. If a task is running, wait for it to finish before changing the connection.
