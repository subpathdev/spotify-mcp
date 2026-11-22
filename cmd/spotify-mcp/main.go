// Command spotify-mcp is an MCP server that controls Spotify playback, with a
// small web page to connect a Spotify account through one-time invite links.
//
//	spotify-mcp [serve]         run the MCP server and the connect page
//	spotify-mcp invite [-ttl]   print a one-time link that connects an account
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
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/subpathdev/spotify-mcp/internal/spotify"
	"github.com/subpathdev/spotify-mcp/internal/store"
	"github.com/subpathdev/spotify-mcp/internal/tools"
	"github.com/subpathdev/spotify-mcp/internal/web"
)

var version = "dev"

type config struct {
	clientID     string
	clientSecret string
	redirectURI  string
	publicURL    string
	dataDir      string
	profile      string
	mcpAddr      string
	webAddr      string
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig(needSecrets bool) (config, error) {
	c := config{
		clientID:     env("SPOTIFY_CLIENT_ID", ""),
		clientSecret: env("SPOTIFY_CLIENT_SECRET", ""),
		redirectURI:  env("SPOTIFY_REDIRECT_URI", ""),
		publicURL:    env("PUBLIC_URL", ""),
		dataDir:      env("DATA_DIR", "/data"),
		profile:      env("PROFILE", "default"),
		mcpAddr:      env("MCP_ADDR", ":8000"),
		webAddr:      env("WEB_ADDR", ":8080"),
	}
	var missing []string
	if c.redirectURI == "" {
		missing = append(missing, "SPOTIFY_REDIRECT_URI")
	}
	if needSecrets {
		if c.clientID == "" {
			missing = append(missing, "SPOTIFY_CLIENT_ID")
		}
		if c.clientSecret == "" {
			missing = append(missing, "SPOTIFY_CLIENT_SECRET")
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing environment variables: %s (see .env.example)", strings.Join(missing, ", "))
	}
	if !strings.HasSuffix(c.redirectURI, "/callback") {
		return c, errors.New("SPOTIFY_REDIRECT_URI must end in /callback, e.g. https://spotify.example.com/callback")
	}
	if c.publicURL == "" {
		c.publicURL = strings.TrimSuffix(c.redirectURI, "/callback")
	}
	c.publicURL = strings.TrimRight(c.publicURL, "/")
	if !store.ValidProfile(c.profile) {
		return c, fmt.Errorf("PROFILE %q: use lower-case letters, digits and dashes", c.profile)
	}
	return c, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
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
	c, err := loadConfig(false)
	if err != nil {
		return err
	}
	st, err := store.Open(c.dataDir)
	if err != nil {
		return err
	}
	code, err := st.CreateInvite(c.profile, *ttl)
	if err != nil {
		return err
	}
	fmt.Printf("%s/connect?invite=%s\n", c.publicURL, code)
	fmt.Fprintf(os.Stderr, "One-time link for profile %q, valid until %s.\n", c.profile, time.Now().Add(*ttl).Format("2006-01-02 15:04 MST"))
	return nil
}

func serve(log *slog.Logger) error {
	c, err := loadConfig(true)
	if err != nil {
		return err
	}
	st, err := store.Open(c.dataDir)
	if err != nil {
		return err
	}
	oc := &oauth2.Config{
		ClientID:     c.clientID,
		ClientSecret: c.clientSecret,
		Endpoint:     spotify.Endpoint,
		RedirectURL:  c.redirectURI,
		Scopes:       spotify.Scopes,
	}
	client := spotify.NewClient(oc, st, c.profile, nil)
	if _, err := st.Token(c.profile); errors.Is(err, store.ErrNotFound) {
		log.Warn("no Spotify account connected yet; run 'spotify-mcp invite' and open the link", "profile", c.profile)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "spotify-mcp", Title: "Spotify", Version: version}, &mcp.ServerOptions{
		Instructions: "Controls Spotify playback on the user's devices (Spotify Connect). Use devices to find a device name, search for URIs, and play/pause/resume/next/previous/transfer to control playback.",
	})
	tools.Register(srv, client)
	mcpMux := http.NewServeMux()
	mcpMux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Logger:         log,
		SessionTimeout: 30 * time.Minute,
	}))
	mcpMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })

	wh := &web.Handler{
		OAuth:     oc,
		Store:     st,
		Log:       log,
		MeURL:     "https://api.spotify.com/v1/me",
		OnConnect: func(string) { client.Forget() },
	}

	servers := []*http.Server{
		{Addr: c.mcpAddr, Handler: mcpMux, ReadHeaderTimeout: 10 * time.Second},
		{Addr: c.webAddr, Handler: wh.Routes(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	log.Info("spotify-mcp started", "version", version, "profile", c.profile, "connect_url", c.publicURL+"/connect")
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	return err
}
