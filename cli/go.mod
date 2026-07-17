module github.com/ratecap/cli

go 1.26.2

replace github.com/ratecap/core => ../services/core

replace github.com/ratecap/sdk-go => ../packages/sdks/go

require (
	github.com/ratecap/core v0.0.0-00010101000000-000000000000
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.45.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
