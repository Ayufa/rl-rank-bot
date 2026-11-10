module rlrankbot

go 1.26.0

require (
	github.com/dank/rlapi v0.1.29
	github.com/fogleman/gg v1.3.0
	github.com/golang/freetype v0.0.0-20170609003504-e2365dfdc4a0
	golang.org/x/image v0.47.0
)

require github.com/gorilla/websocket v1.5.3 // indirect

replace github.com/dank/rlapi => ./third_party/rlapi
