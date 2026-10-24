#!/usr/bin/env python3
"""Secure, interactive Inception onboarding for a running MeadowOS Agent.

The native Agent owns validation, private credential storage, and application
of the selected connection. This presentation helper never edits its catalog.
No API key is accepted in arguments, environment variables, or files.
"""
from __future__ import annotations

import argparse
import getpass
import importlib.util
import json
from pathlib import Path
import re
import sys
import warnings
from urllib import error, request

MODELS_URL = "https://api.inceptionlabs.ai/v1/models"
MODEL_ID = "mercury-3"
MAX_RESPONSE_BYTES = 1024 * 1024
MAX_KEY_BYTES = 16 * 1024


class SetupError(Exception):
    """Only fixed, public messages may be displayed to the user."""


class SafeParser(argparse.ArgumentParser):
    def error(self, message):
        # argparse's default error includes unknown arguments, which could be
        # an accidentally supplied key. Never echo argument values.
        self.print_usage(sys.stderr)
        self.exit(2, "不接受這些參數。API key 請只在隱藏輸入提示中貼上。\n")


class _NoRedirect(request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _valid_key(key):
    return (isinstance(key, str) and 0 < len(key) <= MAX_KEY_BYTES
            and all(33 <= ord(char) <= 126 for char in key))


def read_key():
    if not sys.stdin.isatty():
        raise SetupError("需要互動式終端機，請使用 docker compose exec -it 執行；不接受管線輸入金鑰。")
    try:
        # getpass may otherwise fall back to echoing stdin. Treat that fallback
        # as an error even if stdin claims to be a terminal.
        with warnings.catch_warnings():
            warnings.simplefilter("error", getpass.GetPassWarning)
            key = getpass.getpass("貼上 Inception API key（輸入不會顯示）：")
    except (getpass.GetPassWarning, EOFError, OSError):
        raise SetupError("無法安全關閉輸入回顯；設定已停止，請在互動式終端機重試。") from None
    if not _valid_key(key):
        raise SetupError("API key 為空白或格式不正確；請重新執行並貼上完整金鑰。")
    return key


def verify_provider(key, *, opener=None):
    """Read-only model listing; never follows redirects or displays a body."""
    if not _valid_key(key):
        raise SetupError("API key 格式不正確。")
    opener = opener or request.build_opener(_NoRedirect())
    req = request.Request(MODELS_URL, headers={
        "Authorization": "Bearer " + key,
        "Accept": "application/json",
    }, method="GET")
    try:
        with opener.open(req, timeout=20) as response:
            if response.status != 200:
                raise SetupError("Inception 未接受模型清單請求；請確認金鑰與帳戶狀態後重試。")
            raw = response.read(MAX_RESPONSE_BYTES + 1)
    except error.HTTPError as exc:
        status = exc.code
        exc.close()
        if status in (401, 403):
            raise SetupError("Inception 未授權此金鑰；請確認 API key 與帳戶權限後重試。") from None
        if status == 429:
            raise SetupError("Inception 暫時限制請求；請確認帳戶額度或稍後重試。") from None
        raise SetupError("Inception 模型清單請求未成功；請確認服務與帳戶狀態後重試。") from None
    except (error.URLError, TimeoutError, OSError, ValueError):
        raise SetupError("無法安全連線到 Inception；請確認網路與系統時間後重試。") from None
    if len(raw) > MAX_RESPONSE_BYTES:
        raise SetupError("Inception 模型清單超過安全大小限制；設定尚未儲存。")
    try:
        payload = json.loads(raw.decode("utf-8"))
        rows = payload["data"]
        if not isinstance(rows, list) or len(rows) > 512:
            raise ValueError()
        ids = [row["id"] for row in rows]
        if (any(not isinstance(value, str) or not value or len(value) > 512
                or value.strip() != value or any(ord(c) < 32 for c in value)
                for value in ids) or len(set(ids)) != len(ids)):
            raise ValueError()
    except (ValueError, TypeError, KeyError, UnicodeError):
        raise SetupError("Inception 回傳的模型清單格式不正確；設定尚未儲存。") from None
    if MODEL_ID not in ids:
        raise SetupError("此 Inception 帳戶目前未列出 Mercury 3；設定尚未儲存。")


def configure(client, key, *, verifier=verify_provider):
    """Use the authenticated Unix API and whole-Agent idle lease only."""
    verifier(key)
    try:
        lease = client._request("POST", "/agent/configuration/lease")
    except Exception:
        raise SetupError("Meadow Agent 目前無法取得閒置設定鎖；請等待進行中的工作完成，再重新執行。") from None
    lease_id = lease.get("leaseId") if isinstance(lease, dict) else None
    if not isinstance(lease_id, str) or re.fullmatch(r"[0-9a-f]{64}", lease_id) is None:
        raise SetupError("Meadow Agent 回傳的設定鎖無效；設定尚未儲存。")
    try:
        try:
            result = client._request("POST", "/agent/connections/inception", {
                "leaseId": lease_id, "apiKey": key,
            })
        except Exception:
            raise SetupError("Meadow Agent 未確認設定套用成功；請在工作完成後重新執行。金鑰不會出現在錯誤輸出中。") from None
        if (not isinstance(result, dict) or result.get("applied") is not True
                or result.get("modelId") != MODEL_ID
                or not isinstance(result.get("connectionId"), str)
                or not result["connectionId"]
                or result.get("supportsTools") is not True
                or result.get("supportsImages") is not False
                or result.get("reasoningControl") != "inception-effort"):
            raise SetupError("Meadow Agent 未確認完整的 Mercury 3 工具設定；請重新執行設定。")
        try:
            status = client._request("GET", "/agent/model")
        except Exception:
            raise SetupError("設定可能已儲存，但尚未確認目前模型；請重新執行設定以確認。") from None
        if (not isinstance(status, dict) or status.get("ready") is not True
                or status.get("status", {}).get("model", {}).get("id") != MODEL_ID):
            raise SetupError("設定可能已儲存，但 Mercury 3 尚未成為目前模型；請重新執行設定。")
        return result
    finally:
        try:
            client._request("DELETE", "/agent/configuration/lease", {"leaseId": lease_id})
        except Exception:
            # The native lease expires after 15 seconds. Never print its token
            # or risk replacing a more useful setup error with a raw response.
            pass


def load_agent_client():
    here = Path(__file__).resolve()
    installed = here.parent / "shell/agent_client.py"
    source = here.parents[2] / "packaging/meadowos/shell/agent_client.py"
    paths = [installed, Path("/opt/meadow/share/meadow-os/shell/agent_client.py"), source]
    path = next((candidate for candidate in paths if candidate.is_file()), source)
    if not path.is_file():
        raise SetupError("找不到 MeadowOS 的 Agent 連線元件；請使用完整發行套件。")
    spec = importlib.util.spec_from_file_location("meadow_inception_agent_client", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.AgentClient


def main(argv=None):
    parser = SafeParser(prog="configure_inception.py", description="互動式設定 Inception Mercury 3；API key 僅接受隱藏終端機輸入。")
    parser.parse_args(argv)
    client = None
    try:
        if not sys.stdin.isatty():
            raise SetupError("需要互動式終端機，請使用 docker compose exec -it 執行；不接受管線輸入金鑰。")
        AgentClient = load_agent_client()
        client = AgentClient()
        try:
            client.connect()
        except Exception:
            raise SetupError("無法驗證正在執行的 Meadow Agent；請先啟動 MeadowOS，再執行設定。") from None
        print("即將設定 Inception Mercury 3。金鑰會儲存在此容器的私人持久資料卷。")
        key = read_key()
        try:
            configure(client, key)
        finally:
            key = None
        print("已驗證 Inception 模型清單，並儲存、套用 Mercury 3（工具已啟用）。")
        print("回到 MeadowOS 聊天室，點聊天選單（⋮）→「重新連線」；首次空白對話即可開始。")
        print("已有內容的對話保留原連線；從同一個聊天選單選「新對話」，即可使用剛設定的金鑰。")
        return 0
    except SetupError as exc:
        print("設定未完成：" + str(exc), file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("\n已取消設定。", file=sys.stderr)
        return 130
    except Exception:
        # Imported transports and remote bodies are untrusted display text.
        print("設定未完成：發生未預期錯誤；未輸出金鑰或伺服器內容。", file=sys.stderr)
        return 1
    finally:
        if client is not None:
            client.close()


if __name__ == "__main__":
    sys.exit(main())
