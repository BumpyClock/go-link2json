$env:GOPROXY = "https://proxy.golang.org"
go mod tidy
git add *
git commit -m "go-link2json: changes for v1.0.12"
git tag v1.0.12
git push origin v1.0.12
go list -m github.com/BumpyClock/go-link2json