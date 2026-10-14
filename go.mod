module github.com/jonalfarlinga/bacnet-explorer

go 1.25

require github.com/jonalfarlinga/bacnet v0.0.0

// Until the library's v0.4.0 is tagged, build against the checkout beside
// this one. Then drop this line and go get github.com/jonalfarlinga/bacnet@v0.4.0
replace github.com/jonalfarlinga/bacnet => ../bacnet
