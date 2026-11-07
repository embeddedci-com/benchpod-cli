package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/spf13/cobra"
)

// ── vision: AI Vision boards (BenchPod AI Vision) on embeddedci.com ──
//
// An AI Vision board runs ai-vision-agent: an HTTP API on its LAN (/api/v1). Registering it is the
// pod's flow with the board's own calls: read its key (GET /api/v1/status public_key), register it
// in the signed-in organization (POST /api/aivision/devices), then store the pairing on the board
// (PUT /api/v1/cloud), after which the board connects to embeddedci.com on its own.

func newVisionCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vision",
		Short: "AI Vision boards: register, list, deregister, unpair",
		Long: "Manage AI Vision boards (BenchPod AI Vision) on embeddedci.com.\n\n" +
			"  benchpod vision register <board>   register a board on its LAN and pair it\n" +
			"  benchpod vision list               the organization's boards and whether they are online\n" +
			"  benchpod vision deregister <name>  take a board out of the organization\n" +
			"  benchpod vision unpair <board>     make a board forget its embeddedci.com pairing\n\n" +
			"<board> is the board's LAN address (host, host:port or URL). Sign in first with\n" +
			"`benchpod login` (or set BENCHPOD_API_KEY to a key with the Control AI Vision scope).",
	}
	cmd.AddCommand(newVisionRegisterCmd(g), newVisionListCmd(g), newVisionDeregisterCmd(g), newVisionUnpairCmd(g))
	return cmd
}

type visionServerFlags struct {
	serverURL, tokenFile string
}

func (f *visionServerFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.serverURL, "server-url", defaultServerURL, "embeddedci-server base URL")
	cmd.Flags().StringVar(&f.tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
}

// credential is BENCHPOD_API_KEY or the `benchpod login` session (signing in when there is none).
func (f *visionServerFlags) credential(ctx context.Context) (*serverapi.Client, string, error) {
	if strings.TrimSpace(f.serverURL) == "" {
		return nil, "", errors.New("--server-url cannot be empty")
	}
	tokenPath, err := resolveTokenPath(f.tokenFile)
	if err != nil {
		return nil, "", fmt.Errorf("resolve token path: %w", err)
	}
	api := serverapi.New(f.serverURL)
	cred, err := credentialFor(ctx, api, f.tokenFile, tokenPath)
	if err != nil {
		return nil, "", fmt.Errorf("auth: %w", err)
	}
	return api, cred, nil
}

// ── register ──

type visionRegisterOptions struct {
	visionServerFlags
	board, name, boardToken, pairServer string
	wait                                time.Duration
}

func newVisionRegisterCmd(g *globalFlags) *cobra.Command {
	var o visionRegisterOptions
	cmd := &cobra.Command{
		Use:   "register <board>",
		Short: "Register an AI Vision board on embeddedci.com and pair it",
		Long: "Register the AI Vision board at <board> (its LAN address) in your active organization,\n" +
			"then store the pairing on the board so it connects to embeddedci.com by itself.\n\n" +
			"Running it again is harmless: the same board keeps the same registration. Afterwards the\n" +
			"board shows in the BenchPod workspace on embeddedci.com, and tests reach it from anywhere\n" +
			"as embeddedci:<name> (AIVision(\"embeddedci:<name>\") in the Python SDK).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			o.board = args[0]
			return runVisionRegister(o)
		},
	}
	o.visionServerFlags.add(cmd)
	cmd.Flags().StringVar(&o.name, "name", "", "the board's name on embeddedci.com, URL-safe (default: the board's host name)")
	cmd.Flags().StringVar(&o.boardToken, "board-token", "", "the board's API token, when it has one (also AIVISION_TOKEN)")
	cmd.Flags().StringVar(&o.pairServer, "pair-server", "",
		"host the board connects to (default: api.embeddedci.com for embeddedci.com, else the --server-url host)")
	cmd.Flags().DurationVar(&o.wait, "wait", 30*time.Second, "how long to wait for the board to connect (0 = don't wait)")
	return cmd
}

func runVisionRegister(o visionRegisterOptions) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	board, err := newBoardClient(o.board, o.boardToken)
	if err != nil {
		return err
	}
	var status struct {
		Hostname      string  `json:"hostname"`
		PublicKey     *string `json:"public_key"`
		IdentityError string  `json:"identity_error"`
	}
	if err := board.json(ctx, http.MethodGet, "status", nil, &status); err != nil {
		return fmt.Errorf("read the board's status at %s: %w", board.base, err)
	}
	if status.PublicKey == nil || strings.TrimSpace(*status.PublicKey) == "" {
		if status.IdentityError != "" {
			return fmt.Errorf("the board has no identity key: %s", status.IdentityError)
		}
		return errors.New("the board reports no public_key: update ai-vision-agent to a version with a device key")
	}
	pubKey := strings.TrimSpace(*status.PublicKey)
	name := strings.TrimSpace(o.name)
	if name == "" {
		name = visionDefaultName(status.Hostname, pubKey)
	}

	api, cred, err := o.credential(ctx)
	if err != nil {
		return err
	}
	regCtx, regCancel := context.WithTimeout(ctx, 30*time.Second)
	dev, err := api.RegisterAivisionDevice(regCtx, cred, name, pubKey)
	regCancel()
	if err != nil {
		return fmt.Errorf("register the board: %w", err)
	}
	log.Printf("board: registered name=%s id=%s", dev.Name, dev.ID)
	if dev.Name != name {
		fmt.Fprintf(os.Stderr, "This board is already registered as %q.\n", dev.Name)
	}

	host, port, tls, err := parseServerEndpoint(o.serverURL)
	if err != nil {
		return fmt.Errorf("parse --server-url: %w", err)
	}
	server := visionPairServer(podHostFor(host, o.pairServer), port, tls, o.pairServer != "")
	pair := map[string]string{"server": server, "device_id": dev.ID}
	if err := board.json(ctx, http.MethodPut, "cloud", pair, nil); err != nil {
		return fmt.Errorf("pair the board (PUT /api/v1/cloud): %w", err)
	}
	log.Printf("paired: the board connects to %s as device_id %s", server, dev.ID)

	connection := "embeddedci:" + dev.Name
	if o.wait <= 0 {
		fmt.Fprintf(os.Stderr, "Registered AI Vision board %q (id %s). Connection: %s\n", dev.Name, dev.ID, connection)
		return nil
	}
	st, pollErr := waitCloudConnected(ctx, func(ctx context.Context) (cloudStatus, error) {
		return board.cloudStatus(ctx)
	}, o.wait, time.Second)
	if st.State == "connected" {
		fmt.Fprintf(os.Stderr, "Registered AI Vision board %q and it is connected.\n", dev.Name)
		fmt.Fprintf(os.Stderr, "Use it from anywhere: AIVision(%q) in Python, or open it on the BenchPod page.\n", connection)
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Registered AI Vision board %q, but it did not connect to %s within %s.\n", dev.Name, server, o.wait)
	fmt.Fprintln(&b, "The registration and the pairing stand; the board keeps retrying on its own.")
	switch {
	case st.LastError != nil && *st.LastError != "":
		fmt.Fprintf(&b, "The board's last error: %s\n", *st.LastError)
	case pollErr != nil:
		fmt.Fprintf(&b, "Reading the board's connection state failed: %v\n", pollErr)
	default:
		fmt.Fprintln(&b, "The board reports no error yet: its agent may not have the embeddedci.com connection.")
	}
	fmt.Fprintf(&b, "Check it with: curl %s/api/v1/cloud", board.base)
	return errors.New(b.String())
}

// visionDefaultName is the board's host name made URL-safe, else "ai-vision-" and the first three
// bytes of its key.
func visionDefaultName(hostname, publicKey string) string {
	if h := strings.TrimSpace(hostname); h != "" {
		if n := strings.ToLower(defaultDeviceNameFromAddr(strings.SplitN(h, ".", 2)[0])); n != "" && n != "benchpod" {
			return n
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(publicKey), "="))
	if err != nil || len(raw) < 3 {
		return "ai-vision"
	}
	return fmt.Sprintf("ai-vision-%02x%02x%02x", raw[0], raw[1], raw[2])
}

// visionPairServer is the "server" the board stores: the host name, with the port when it is not
// the scheme's default (a test server). An explicit --pair-server is taken as given.
func visionPairServer(host string, port int, tls, explicit bool) string {
	if explicit || port == 0 || (tls && port == 443) || (!tls && port == 80) {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// ── list ──

func newVisionListCmd(g *globalFlags) *cobra.Command {
	var f visionServerFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "The organization's AI Vision boards and whether each is online",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx := context.Background()
			api, cred, err := f.credential(ctx)
			if err != nil {
				return err
			}
			boards, err := api.ListAivisionDevices(ctx, cred)
			if err != nil {
				return fmt.Errorf("list boards: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(boards)
			}
			if len(boards) == 0 {
				fmt.Fprintln(out, "No AI Vision boards. Register one with: benchpod vision register <board address>")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATE\tCONNECTION\tID")
			for _, b := range boards {
				state := "offline"
				if b.Online {
					state = "online"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Name, state, b.Connection, b.ID)
			}
			return tw.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the boards as JSON")
	return cmd
}

// ── deregister ──

func newVisionDeregisterCmd(g *globalFlags) *cobra.Command {
	var f visionServerFlags
	var unpairBoard, boardToken string
	cmd := &cobra.Command{
		Use:   "deregister <name or id>",
		Short: "Take an AI Vision board out of the organization",
		Long: "Deregister the AI Vision board: it is disconnected and its logins are refused until it is\n" +
			"registered again. --unpair <board address> also makes the board forget its pairing.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			api, cred, err := f.credential(ctx)
			if err != nil {
				return err
			}
			boards, err := api.ListAivisionDevices(ctx, cred)
			if err != nil {
				return fmt.Errorf("list boards: %w", err)
			}
			ref := strings.TrimSpace(args[0])
			var dev *serverapi.AivisionDevice
			for i := range boards {
				if boards[i].ID == ref || boards[i].Name == ref {
					dev = &boards[i]
					break
				}
			}
			if dev == nil {
				return fmt.Errorf("no AI Vision board named %q in this organization (benchpod vision list shows them)", ref)
			}
			if _, err := api.DeregisterAivisionDevice(ctx, cred, dev.ID); err != nil {
				return fmt.Errorf("deregister %s: %w", dev.Name, err)
			}
			fmt.Fprintf(os.Stderr, "Deregistered AI Vision board %q (id %s).\n", dev.Name, dev.ID)
			if strings.TrimSpace(unpairBoard) == "" {
				return nil
			}
			board, err := newBoardClient(unpairBoard, boardToken)
			if err != nil {
				return err
			}
			if err := board.json(ctx, http.MethodDelete, "cloud", nil, nil); err != nil {
				return fmt.Errorf("unpair the board at %s: %w", board.base, err)
			}
			fmt.Fprintln(os.Stderr, "The board forgot its pairing.")
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&unpairBoard, "unpair", "", "the board's LAN address: also make it forget its pairing")
	cmd.Flags().StringVar(&boardToken, "board-token", "", "the board's API token, when it has one (also AIVISION_TOKEN)")
	return cmd
}

// ── unpair ──

func newVisionUnpairCmd(g *globalFlags) *cobra.Command {
	var boardToken string
	cmd := &cobra.Command{
		Use:   "unpair <board>",
		Short: "Make an AI Vision board forget its embeddedci.com pairing (on its LAN)",
		Long: "The board stops connecting to embeddedci.com. It stays registered there until\n" +
			"`benchpod vision deregister`.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			board, err := newBoardClient(args[0], boardToken)
			if err != nil {
				return err
			}
			if err := board.json(context.Background(), http.MethodDelete, "cloud", nil, nil); err != nil {
				return fmt.Errorf("unpair the board at %s: %w", board.base, err)
			}
			fmt.Fprintf(os.Stderr, "The board at %s forgot its pairing.\n", board.base)
			return nil
		},
	}
	cmd.Flags().StringVar(&boardToken, "board-token", "", "the board's API token, when it has one (also AIVISION_TOKEN)")
	return cmd
}

// ── the board's LAN API ──

type boardClient struct {
	base, token string
	http        *http.Client
}

// newBoardClient takes host, host:port or a URL (http on port 80 when no scheme is given).
func newBoardClient(addr, token string) (*boardClient, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		return nil, errors.New("the board's address is empty")
	}
	if strings.HasPrefix(strings.ToLower(a), "embeddedci:") {
		return nil, fmt.Errorf("%q is a cloud name; this needs the board's LAN address", a)
	}
	if !strings.Contains(a, "://") {
		a = "http://" + a
	}
	u, err := url.Parse(a)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("not a board address: %q", addr)
	}
	if strings.TrimSpace(token) == "" {
		token = os.Getenv("AIVISION_TOKEN")
	}
	return &boardClient{base: u.Scheme + "://" + u.Host, token: strings.TrimSpace(token), http: &http.Client{Timeout: 15 * time.Second}}, nil
}

// json calls the board's /api/v1/<path> and decodes a 2xx answer into out (when not nil). An error
// answer ({"error": {"code", "message"}}) becomes its message.
func (b *boardClient) json(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+"/api/v1/"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			if resp.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("%s (pass --board-token or set AIVISION_TOKEN)", e.Error.Message)
			}
			return fmt.Errorf("%s (%s)", e.Error.Message, e.Error.Code)
		}
		return fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// cloudStatus reads GET /api/v1/cloud as the pod's cloud_status shape.
func (b *boardClient) cloudStatus(ctx context.Context) (cloudStatus, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var c struct {
		Connected bool   `json:"connected"`
		LastError string `json:"last_error"`
	}
	if err := b.json(cctx, http.MethodGet, "cloud", nil, &c); err != nil {
		return cloudStatus{}, err
	}
	st := cloudStatus{State: "disconnected"}
	if c.Connected {
		st.State = "connected"
	}
	if c.LastError != "" {
		st.LastError = &c.LastError
	}
	return st, nil
}
