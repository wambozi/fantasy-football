# Draft Copilot — one binary, no build step for the UI.
DATA ?= ./data
PORT ?= 8090

.PHONY: build ingest sim test run keepers brief-test clean

build:            ## build ./server (UI is embedded from web/)
	go build -o server ./cmd/server

ingest:           ## data/*.csv -> data/players.json
	go run ./cmd/ingest -data $(DATA)

sim:              ## 500 mock drafts; exits non-zero if a §10 invariant fails
	go run ./cmd/simdraft -data $(DATA) -n 500 -sims 400

keepers:          ## list the 2027 keeper-speculative board
	go run ./cmd/simdraft -data $(DATA) -keepers

test:
	go test ./...

run: build        ## start on 0.0.0.0:$(PORT)
	./server -port $(PORT) -data $(DATA)

brief-test: build ## one real Claude call for the current board (needs FANTASY_ANTHROPIC_API_KEY)
	./server -data $(DATA) -brief-test

clean:
	rm -f server
