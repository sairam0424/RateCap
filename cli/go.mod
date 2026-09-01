module github.com/sairam0424/RateCap/cli

go 1.26.2

replace github.com/sairam0424/RateCap/services/core => ../services/core

replace github.com/sairam0424/RateCap/packages/sdks/go => ../packages/sdks/go

require (
	github.com/sairam0424/RateCap/services/core v0.0.0-00010101000000-000000000000
	github.com/sairam0424/RateCap/packages/sdks/go v0.0.0-00010101000000-000000000000
	github.com/spf13/cobra v1.10.2
	golang.org/x/time v0.15.0
)

require (
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
