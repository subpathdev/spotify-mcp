// Command spotify-mcp is an MCP server that controls Spotify playback, with a
// small web page to connect a Spotify account through one-time invite links.
//
//	spotify-mcp [serve]         run the MCP server and the connect page
//	spotify-mcp invite [-ttl]   print a one-time link that connects an account
//	spotify-mcp version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/account"
	"github.com/subpathdev/spotify-mcp/internal/config"
	"github.com/subpathdev/spotify-mcp/internal/filestore"
	"github.com/subpathdev/spotify-mcp/internal/mcpserver"
	"github.com/subpathdev/spotify-mcp/internal/playback"
	"github.com/subpathdev/spotify-mcp/internal/spotify"
	"github.com/subpathdev/spotify-mcp/internal/web"
)

// version is set at build time.
var version = "dev"

func main() {
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	case "invite":
		err = invite(args)
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (use serve, invite or version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func invite(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	ttl := fs.Duration("ttl", 24*time.Hour, "how long the link stays valid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	store, err := filestore.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	code, expires, err := account.NewInviter(store).CreateInvite(context.Background(), cfg.ProfileID, *ttl)
	if err != nil {
		return err
	}
	fmt.Println(cfg.InviteURL(code))
	fmt.Fprintf(os.Stderr, "One-time link for profile %q, valid until %s.\n", cfg.ProfileID, expires.Format("2006-01-02 15:04 MST"))
	return nil
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	store, err := filestore.Open(cfg.DataDir)
	if err != nil {
		return err
	}

	oauth := spotify.NewOAuth(spotify.OAuthConfig{
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL.String(),
	})
	api := spotify.NewAPI(spotify.DefaultAPIURL, nil)
	tokens := account.NewTokenSource(store, oauth, cfg.ProfileID)
	player := playback.NewService(spotify.NewPlayer(api, tokens))
	accounts := account.NewService(store, store, oauth, api)

	// Only for the log: a broken token file shouldn't stop the server, the
	// tools report it when they need the token.
	prof, err := account.NewProfiles(store).Profile(context.Background(), cfg.ProfileID)
	if err != nil {
		log.Warn("can't read the connected account", "profile", cfg.ProfileID.String(), "err", err)
	} else if acc, ok := prof.Account(); ok {
		log.Info("spotify account connected", "profile", prof.ID().String(), "spotify_user", acc.ID)
	} else {
		log.Warn("no Spotify account connected yet; run 'spotify-mcp invite' and open the link", "profile", prof.ID().String())
	}

	servers := []*http.Server{
		{
			Addr:              cfg.MCPAddr,
			Handler:           mcpserver.Handler(mcpserver.NewServer(player, version), log),
			ReadHeaderTimeout: 10 * time.Second,
		},
		{
			Addr:              cfg.WebAddr,
			Handler:           web.NewHandler(accounts, log),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
		},
	}
	log.Info("spotify-mcp started", "version", version, "profile", cfg.ProfileID.String(), "connect_url", cfg.PublicURL.JoinPath("connect").String())
	return run(log, servers)
}

// run serves until SIGINT/SIGTERM or a server fails, then shuts all down.
func run(log *slog.Logger, servers []*http.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("serve %s: %w", s.Addr, err)
			}
		}()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if serr := s.Shutdown(shutdown); serr != nil {
			log.Warn("shutdown", "addr", s.Addr, "err", serr)
		}
	}
	return err
}
