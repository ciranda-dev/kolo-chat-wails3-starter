# Kolo Chat, Wails 3 starter

[![status: beta](https://img.shields.io/badge/status-beta-orange)](CHANGELOG.md)
[![license: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![go: 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8)](go.mod)
[![wails: 3](https://img.shields.io/badge/wails-3-DF0000)](go.mod)

The scaffold for the "Build a desktop chat app" tutorial in the [Kolo docs](https://docs.kolo.ciranda.dev/build/desktop-chat/). The window (React, JavaScript), the build config, the icons and the bootstrap node in `cmd/bootstrap` are done. `chatservice.go` is an empty service: the tutorial fills it in with [Kolo](https://github.com/ciranda-dev). It builds and opens a window that waits for a peer.

Requirements: Go 1.26+, Node.js, and the `wails3` CLI (`go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0`).

Start with the tutorial. The finished version is the kolo-chat-wails3-example repository.

## What the starter sets for your app

- **Content security policy.** `frontend/index.html` allows only the app's own scripts, styles and images (no remote address, no inline script, no object). Keep it when you add a Markdown or link renderer: it is the second line of defense after rendering text as text. `npm run lint` in `frontend` fails when the policy is missing, when the page has an inline script or style, and on remote addresses or HTML built from strings; `npm test` runs its tests. The dev server (`wails3 dev`) serves the page without the policy, because hot reload needs inline scripts.
- **A bounded message list.** The window keeps the last 500 messages and cuts a message to 2000 characters and a sender to 64 (`frontend/src/messages.js`), so a peer cannot grow the page without limit.
- **A soft memory limit.** The app sets 2 GiB at start (`memlimit.go`) unless `GOMEMLIMIT` is set, which wins.
- **Generated bindings.** `frontend/bindings` is made by `wails3 build` and `wails3 dev` (`wails3 generate bindings`) and is overwritten each time; it is tracked here only because the `step-2` tag shows it.

## Build a release

The version comes from the git tag when the app is built: `wails3 build` puts it into the binary, and a development build (`wails3 dev`) says `dev`. The packaging files (`build/config.yml`, the plists, the deb, the Windows and MSIX manifests, the installer) hold the placeholder `0.0.0`; stamp the version into a copy of the repository before packaging it:

```sh
KOLO_VERSION=v1.2.3 wails3 build                  # or leave KOLO_VERSION out to use the git tag
scripts/stamp-version.sh v1.2.3 /path/to/a/copy   # then wails3 package in that copy
```

## Tags

The tags `step-2` to `step-8` hold the code of each step of the tutorial, to compare with your own files or to bring in the window of a step (`git checkout step-3 -- frontend`). They live on the `steps` branch, so `main` stays the starter.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
