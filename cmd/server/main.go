// Command server runs the draft copilot: one binary, SPA embedded, state in memory.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wambozi/draft-copilot/internal/brief"
	"github.com/wambozi/draft-copilot/internal/engine"
	"github.com/wambozi/draft-copilot/internal/httpapi"
	"github.com/wambozi/draft-copilot/internal/league"
	"github.com/wambozi/draft-copilot/internal/players"
	"github.com/wambozi/draft-copilot/internal/state"
	"github.com/wambozi/draft-copilot/internal/strategy"
	"github.com/wambozi/draft-copilot/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port      = flag.Int("port", 8090, "listen port (binds 0.0.0.0)")
		dataDir   = flag.String("data", "./data", "data directory")
		eventsLog = flag.String("events", "", "event log path (default <data>/events.jsonl)")
		myTeam    = flag.String("team", "Sittin Purdy", "my team name as it appears in draft-order.csv")
		printOnly = flag.Bool("print-picks", false, "print derived live picks and exit")
		useBrief  = flag.Bool("brief", true, "enable Claude briefs when FANTASY_ANTHROPIC_API_KEY/ANTHROPIC_API_KEY is set")
		briefTest = flag.Bool("brief-test", false, "make one real brief call for the current state, print it, and exit")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	cfg, err := strategy.Load(filepath.Join(*dataDir, "strategy.yaml"))
	if err != nil {
		return fmt.Errorf("load strategy: %w", err)
	}
	lg, err := league.Load(filepath.Join(*dataDir, "draft-order.csv"), *myTeam, cfg.LeagueRoster())
	if err != nil {
		return fmt.Errorf("load league: %w", err)
	}
	if err := engine.CheckGates(lg, cfg); err != nil {
		slog.Error("strategy.yaml", "err", err)
		os.Exit(1)
	}
	if *printOnly {
		fmt.Printf("teams: %d  slots: %d  keepers: %d  live picks: %d\n", len(lg.Teams), len(lg.Slots), len(lg.Slots)-lg.NumLive(), lg.NumLive())
		fmt.Printf("%s live picks: %v\n", lg.MyTeam, lg.MyLivePicks)
		for _, t := range lg.Teams {
			n := 0
			for _, l := range lg.LiveSlots {
				if lg.Slots[l].Team == t {
					n++
				}
			}
			fmt.Printf("  %-30s keepers=%d live=%d\n", t, len(lg.KeepersByTeam[t]), n)
		}
		return nil
	}

	pool, err := players.Load(filepath.Join(*dataDir, "players.json"))
	if err != nil {
		return fmt.Errorf("load players: %w", err)
	}
	if err := attachKeepers(lg, pool, log); err != nil {
		return err
	}
	if pool.CurveProjections() {
		log.Warn("no projections in players.json — VOR uses a fitted ADP curve (spec §4)")
	}
	log.Info("loaded", "players", len(pool.Players), "teams", len(lg.Teams), "live_picks", lg.NumLive(), "my_picks", lg.MyLivePicks)

	logPath := *eventsLog
	if logPath == "" {
		logPath = filepath.Join(*dataDir, "events.jsonl")
	}
	st, err := state.New(lg, pool, logPath)
	if err != nil {
		return fmt.Errorf("init state: %w", err)
	}
	snap := st.Snapshot()
	log.Info("state ready", "version", snap.Version, "picks_replayed", len(snap.Picks), "on_clock", snap.OnClock, "live_pick", snap.LivePick)

	eng := engine.New(lg, pool, cfg, uint64(time.Now().UnixNano()))
	// Warm the cache so the first page load is instant.
	eng.Advise(snap)
	var briefer httpapi.Briefer
	if key := brief.KeyFromEnv(); key != "" && *useBrief {
		gen := brief.NewAnthropic(key, "")
		svc := brief.New(gen, eng, lg, pool, cfg, brief.Options{Log: log, Poke: st.Poke})
		briefer = svc
		log.Info("claude briefs enabled", "model", gen.Model())
		if *briefTest {
			return briefSmokeTest(gen, lg, pool, cfg, eng, snap)
		}
		go svc.OnPick(snap) // prefetch for the state we booted into
	} else {
		log.Info("claude briefs disabled", "reason", map[bool]string{true: "flag", false: "no FANTASY_ANTHROPIC_API_KEY / ANTHROPIC_API_KEY"}[!*useBrief])
	}

	// Refuse to share a port. Go can bind *:PORT even when another process holds
	// 127.0.0.1:PORT (dual-stack), and then the browser lands on the other app.
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", *port), 300*time.Millisecond); err == nil {
		c.Close()
		return fmt.Errorf("port %d is already in use by another process (lsof -nP -iTCP:%d); pick another with -port", *port, *port)
	}

	srv := httpapi.New(lg, pool, st, httpapi.Options{
		Log:      log,
		Advisor:  adviser{eng},
		Briefer:  briefer,
		Config:   cfg,
		FrameLog: filepath.Join(*dataDir, "fandraft-frames.jsonl"),
		Static:   web.FS,
		Search: func(q string, snap state.Snapshot, limit int) []*players.Player {
			return pool.Search(q, func(p *players.Player) bool { _, taken := snap.Taken[p.ID]; return taken }, limit)
		},
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return httpapi.Serve(ctx, fmt.Sprintf("0.0.0.0:%d", *port), srv.Handler(), log)
}

// briefSmokeTest makes one real call so the key, model and prompt can be verified
// before draft night without clicking through the UI.
func briefSmokeTest(gen *brief.Anthropic, lg *league.League, pool *players.Pool, cfg *strategy.Config, eng *engine.Engine, snap state.Snapshot) error {
	ad := eng.AdviseFor(snap, lg.MyTeam)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	t0 := time.Now()
	text, err := gen.Generate(ctx, brief.SystemPrompt(lg, cfg), brief.UserMessage(snap, ad, lg, pool, false))
	if err != nil {
		return fmt.Errorf("brief call failed: %w", err)
	}
	fmt.Printf("model %s, %dms\n%s\n", gen.Model(), time.Since(t0).Milliseconds(), text)
	return nil
}

// adviser adapts *engine.Engine to the httpapi.Advisor interface (which returns any).
type adviser struct{ e *engine.Engine }

func (a adviser) Advise(snap state.Snapshot) any { return a.e.Advise(snap) }

// attachKeepers resolves each keeper slot to a pool player by exact slug, then by
// normalised name + position. An unresolved keeper is a data bug (the ADP export and
// the draft order disagree) and fails boot rather than being papered over.
func attachKeepers(lg *league.League, pool *players.Pool, log *slog.Logger) error {
	var errs []string
	byName := map[string]*players.Player{}
	for _, p := range pool.Players {
		byName[players.NameKey(p.Name)+"|"+string(p.Pos)] = p
	}
	for i := range lg.Slots {
		s := &lg.Slots[i]
		if s.KeeperID == "" {
			continue
		}
		if p, ok := pool.ByID[s.KeeperID]; ok {
			p.Keeper = true
			continue
		}
		if p, ok := byName[players.NameKey(s.KeeperName)+"|"+string(s.KeeperPos)]; ok {
			log.Info("keeper matched by name", "keeper", s.KeeperName, "id", p.ID)
			s.KeeperID = p.ID
			p.Keeper = true
			continue
		}
		errs = append(errs, fmt.Sprintf("%s (%s %s) at %d.%d", s.KeeperName, s.KeeperPos, s.KeeperTeam, s.Round, s.PickInRound))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d keeper(s) not found in players.json — fix data/adp.csv or the draft order: %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}
