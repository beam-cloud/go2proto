# go2proto

Generate Protobuf messages from given go structs. No RPC, not gogo syntax, just pure Protobuf messages.

### Syntax

```
-f string
    Protobuf output file path. (default ".")
-filter string
    Filter by struct names. Case insensitive.
-p value
    Fully qualified path of packages to analyse. Relative paths ("./example/in") are allowed.
```

### Example

Your package you wish to export must be inside of your working directory. Package paths can be fully-qualified or relative.

```sh
GO111MODULE=off go get -u github.com/beam-cloud/go2proto
cd ~/go/src/github.com/beam-cloud/go2proto
go2proto -f ./example/out -p ./example/in
```

### Note

Generated code may not be perfect but since it just 180 lines of code you are free to adapt it for your needs.
