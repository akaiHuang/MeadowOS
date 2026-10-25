# Get started with MeadowOS

Run MeadowOS locally with Docker, then open the desktop in your browser. The Alpha supports Linux `amd64` and `arm64`; macOS and Windows users can use Docker Desktop.

## What you need

- Git and Docker with Compose v2.
- Several GB of free disk space and memory for the first build.
- Available local ports **60511** and **60531**.

## 1. Build and start

```sh
git clone https://github.com/akaiHuang/MeadowOS.git
cd MeadowOS
docker compose build --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)"
docker compose up -d
```

The first build downloads packages and compiles MeadowOS, so it takes a few minutes. Build for your computer's native architecture.

## 2. Open your desktop

Open [MeadowOS](http://127.0.0.1:60511/vnc.html?autoconnect=true&resize=scale) in your browser.

You can explore Files, Terminal and Settings before connecting an AI model. Keep `127.0.0.1` and the default ports in the address so text input and image attachments work correctly.

## 3. Connect Mercury 3

[Create an Inception API key and connect it to MeadowOS](INCEPTION.md).

After setup, open the chat menu **⋮ → 重新連線 (Reconnect)**. If you already have a conversation or are changing keys, choose **新對話 (New conversation)**.

Try a small request:

> Create a file named hello.txt in my workspace, containing Hello from MeadowOS.

## Stop and return later

```sh
docker compose stop
```

To start again:

```sh
docker compose up -d
```

Your work and conversations remain available when you return.

## Troubleshooting

If the desktop does not appear, check whether MeadowOS is running:

```sh
docker compose ps
docker compose logs --tail=100 meadowos
```

Wait for startup to finish, then reload the desktop page. If Docker reports a port conflict, check that ports 60511 and 60531 are free before starting another instance.

## Alpha limitations

- MeadowOS currently runs as a Docker/X11 desktop. A bootable installer is not available yet.
- GNOME Web can be blocked by Docker's sandbox restrictions. This does not prevent using the desktop chat, Files, Terminal or Settings.
- VoiceOS includes its visual interface; Linux speech recognition and voice playback are not available in this build.
- Mercury 3 supports text and tools. Image understanding is not available with the default connection.

For the system design and components, see [Architecture](ARCHITECTURE.md).
