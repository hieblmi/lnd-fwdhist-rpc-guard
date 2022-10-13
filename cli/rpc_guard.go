package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/urfave/cli"
	macaroon "gopkg.in/macaroon.v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

const (
	defaultRPCPort         = "10009"
	defaultRPCHostPort     = "localhost:" + defaultRPCPort
	FwdingHistoryCaveat_1d = "1d_FwdingHistory"
	FwdingHistoryCaveat_1w = "1w_FwdingHistory"
)

var (
	defaultLndDir      = cleanAndExpandPath("~/.lnd")
	defaultMacaroonDir = "/data/chain/bitcoin/regtest"

	// maxMsgRecvSize is the largest message our client will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)
)

func main() {
	app := &cli.App{
		Name: "guard",
		Usage: "Intercepts `lncli fwdinghistory` calls, checks " +
			"the custom macaroon caveat and replaces the response with" +
			" the appropriate forwarding history report",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "window",
				Value: "-1d",
				Usage: "How many days of forwarding history should be retrieved?" +
					"Possible values are -1d(past day), -1w(past week).",
			},
			&cli.StringFlag{
				Name:  "macaroon",
				Value: defaultLndDir + defaultMacaroonDir + "/admin.macaroon",
				Usage: "admin macaroon for this lnd instance",
			},
			&cli.StringFlag{
				Name:  "cert",
				Value: defaultLndDir + "/tls.cert",
				Usage: "tls certificate for this lnd instance",
			},
			&cli.StringFlag{
				Name:  "host",
				Value: defaultRPCHostPort,
				Usage: "host:port of this lnd's rpc instance, e.g. localhost:10009",
			},
		},
		Action: fwdingHistoryGuard,
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

// fwdingHistoryGuard registers RPC middleware that returns
// forwarding events within a given time frame, e.g. -1d or -1w
func fwdingHistoryGuard(cliCtx *cli.Context) error {

	var window string
	var macaroon string
	var cert string
	var host string

	switch {
	case cliCtx.IsSet("window"):
		window = cliCtx.String("window")
		fmt.Println("window is set: ", window)
	case cliCtx.String("window") != "":
		fmt.Println("Default window is: ", cliCtx.String("window"))
		window = cliCtx.String("window")
	default:
		return fmt.Errorf("Look up window required, please in --window")
	}

	switch {
	case cliCtx.IsSet("macaroon"):
		macaroon = cliCtx.String("macaroon")
		fmt.Println("Macaroon is set: ", macaroon)
	case cliCtx.String("macaroon") != "":
		fmt.Println("Default macaroon is: ", cliCtx.String("macaroon"))
		macaroon = cliCtx.String("macaroon")
	default:
		return fmt.Errorf("macaroon required, please specify absolute path in --macaroon")
	}

	switch {
	case cliCtx.IsSet("cert"):
		cert = cliCtx.String("cert")
		fmt.Println("Cert is set: ", cert)
	case cliCtx.String("cert") != "":
		fmt.Println("Default cert is: ", cliCtx.String("cert"))
		cert = cliCtx.String("cert")
	default:
		return fmt.Errorf("tls cert required, please specify absolute path in --cert")
	}
	switch {
	case cliCtx.IsSet("host"):
		host = cliCtx.String("host")
		fmt.Println("Host is set: ", host)
		break
	case cliCtx.String("host") != "":
		fmt.Println("Default host:port is: ", cliCtx.String("host"))
		host = cliCtx.String("host")
	default:
		return fmt.Errorf("RPC host:port required, please specify host:port in --host")
	}

	fmt.Println("Starting forwarding history guard...")

	conn, err := getClientConn(macaroon, cert, host)
	if err != nil {
		e := "Couldn't establish client connection to lnd." +
			" Please check macaroon/cert/host. \n%w"
		return fmt.Errorf(e, err)
	}
	client := lnrpc.NewLightningClient(conn)
	rpcMiddlewareClient, err := client.RegisterRPCMiddleware(context.Background())
	if err != nil {
		return fmt.Errorf("Couldn't register guard RPC middleware %w", err)
	}

	forwardingWindow := FwdingHistoryCaveat_1d
	if window == "-1w" {
		forwardingWindow = FwdingHistoryCaveat_1w
	}

	// Register interceptor immediately
	err = rpcMiddlewareClient.Send(&lnrpc.RPCMiddlewareResponse{
		MiddlewareMessage: &lnrpc.RPCMiddlewareResponse_Register{
			Register: &lnrpc.MiddlewareRegistration{
				MiddlewareName:           "FwdingHistoryGuard",
				CustomMacaroonCaveatName: forwardingWindow,
				ReadOnlyMode:             false,
			},
		},
	})
	if err != nil {
		panic(err)
	}

	fmt.Println("Registered middleware stream")

	fmt.Println("Listening to middleware stream")

	for {
		resp, err := rpcMiddlewareClient.Recv()
		if err != nil {
			panic(err)
		}

		fwdingHistory := GetFwdingHistory(client, context.Background(), window)

		res, err := proto.Marshal(fwdingHistory)

		err = rpcMiddlewareClient.Send(&lnrpc.RPCMiddlewareResponse{
			RefMsgId: resp.GetMsgId(),
			MiddlewareMessage: &lnrpc.RPCMiddlewareResponse_Feedback{
				Feedback: &lnrpc.InterceptFeedback{
					Error:                 "",
					ReplaceResponse:       true,
					ReplacementSerialized: res,
				},
			},
		})
		if err != nil {
			panic(err)
		}
	}

}

func GetFwdingHistory(client lnrpc.LightningClient, ctx context.Context, window string) *lnrpc.ForwardingHistoryResponse {
	now := time.Now()
	startTime, _ := parseTime(window, now)

	req := &lnrpc.ForwardingHistoryRequest{
		StartTime: startTime,
	}
	resp, err := client.ForwardingHistory(ctx, req)
	if err != nil {
		panic(err)
	}
	return resp
}

// reTimeRange matches systemd.time-like short negative timeranges, e.g. "-200s".
var reTimeRange = regexp.MustCompile(`^-\d{1,18}[s|m|h|d|w|M|y]$`)

// secondsPer allows translating s(seconds), m(minutes), h(ours), d(ays),
// w(eeks), M(onths) and y(ears) into corresponding seconds.
var secondsPer = map[string]int64{
	"s": 1,
	"m": 60,
	"h": 3600,
	"d": 86400,
	"w": 604800,
	"M": 2630016,  // 30.44 days
	"y": 31557600, // 365.25 days
}

// parseTime parses UNIX timestamps or short timeranges inspired by systemd
// (when starting with "-"), e.g. "-1M" for one month (30.44 days) ago.
func parseTime(s string, base time.Time) (uint64, error) {
	if reTimeRange.MatchString(s) {
		last := len(s) - 1

		d, err := strconv.ParseInt(s[1:last], 10, 64)
		if err != nil {
			return uint64(0), err
		}

		mul := secondsPer[string(s[last])]
		return uint64(base.Unix() - d*mul), nil
	}

	return strconv.ParseUint(s, 10, 64)
}

// getClientConn returns a rpc client instance to the caller
func getClientConn(maca string, cert string, host string) (*grpc.ClientConn, error) {

	// Load the specified TLS certificate and build transport credentials
	// with it.
	creds, err := credentials.NewClientTLSFromFile(cert, "")
	if err != nil {
		return nil, err
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	// Only process macaroon credentials if --no-macaroons isn't set and
	// if we're not skipping macaroon processing.
	// Load the specified macaroon file.
	macBytes, err := ioutil.ReadFile(maca)
	if err != nil {
		return nil, fmt.Errorf("unable to read macaroon path (check "+
			"the network setting!): %v", err)
	}

	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("unable to decode macaroon: %v", err)
	}

	// Now we append the macaroon credentials to the dial options.
	cred, err := macaroons.NewMacaroonCredential(mac)
	opts = append(opts, grpc.WithPerRPCCredentials(cred))

	// We need to use a custom dialer so we can also connect to unix sockets
	// and not just TCP addresses.
	genericDialer := lncfg.ClientAddressDialer(strings.Split(host, ":")[1])
	opts = append(opts, grpc.WithContextDialer(genericDialer))
	opts = append(opts, grpc.WithDefaultCallOptions(maxMsgRecvSize))

	conn, err := grpc.Dial(host, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return conn, nil
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
// This function is taken from https://github.com/btcsuite/btcd
func cleanAndExpandPath(path string) string {
	if path == "" {
		return ""
	}

	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		var homeDir string
		user, err := user.Current()
		if err == nil {
			homeDir = user.HomeDir
		} else {
			homeDir = os.Getenv("HOME")
		}

		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but the variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}
