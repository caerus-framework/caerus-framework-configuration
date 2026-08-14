module github.com/caerus-framework/caerus-framework-configuration

go 1.26

require (
	github.com/caerus-framework/caerus-framework v0.0.9
	github.com/caerus-framework/caerus-framework-logs v0.0.7
	github.com/fsnotify/fsnotify v1.10.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	golang.org/x/mod v0.39.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

tool github.com/caerus-framework/caerus-framework/cmd/caerusvet

replace github.com/caerus-framework/caerus-framework => ../caerus-framework

replace github.com/caerus-framework/caerus-framework-logs => ../caerus-framework-logs
