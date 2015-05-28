PKG  = github.com/DevMine/crawld
EXEC = crawld

all: check test build

install:
	go install ${PKG}

build:
	go build -o ${EXEC} ${PKG}

test:
	go test -v -timeout 2m ${PKG}/...

# FIXME: we shall compile libgit2 statically with git2go to prevent libgit2
# from being a dependency to run crawld
deps:
	go get -u github.com/libgit2/git2go
	go get -u golang.org/x/oauth2
	go get -u golang.org/x/net/context
	go get -u github.com/golang/glog
	go get -u github.com/google/go-github/github
	go get -u github.com/google/go-querystring/query
	go get -u github.com/lib/pq

dev-deps:
	go get -u github.com/golang/lint/golint
	go get -u rsc.io/grind

check:
	go vet ${PKG}/...
	golint ${PKG}/...
	grind -diff ${PKG}

cover:
	go test -cover ${PKG}/...

clean:
	rm -f ./${EXEC}
