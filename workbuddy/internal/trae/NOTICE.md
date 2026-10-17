# Source attribution

`internal/auth`, `internal/upstream/{client,constants,headers,payload}.go` and
`internal/login/callback.go` are adapted from the bundled `trae2api` project (MIT,
copyright 2026 connectedGraph). The original license is retained in LICENSE.

The C ABI and host callback integration follow the MIT licensed
`codebuddy-intl-cpa/workbuddy` implementation by zidanefaqih, based on
Sliverkiss/cpa-plugin and lovingfish/workbuddy. CPA API types are provided by
github.com/router-for-me/CLIProxyAPI/v8 (v8.0.23). These projects retain their original copyrights.

New CPA integration, SSE adapter, management and tests are provided under MIT.
