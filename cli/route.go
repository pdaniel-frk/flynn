package main

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/go-docopt"
	"github.com/flynn/flynn/controller/client"
	"github.com/flynn/flynn/router/types"
)

func init() {
	register("route", runRoute, `
usage: flynn route
       flynn route add http [-s <service>] [-c <tls-cert> -k <tls-key>] [--sticky] <domain>
       flynn route add tcp [-s <service>] [-p <port>]
       flynn route update <id> [-s <service>] [-c <tls-cert> -k <tls-key>] [--sticky] [--no-sticky]
       flynn route remove <id>

Manage routes for application.

Options:
	-s, --service=<service>    service name to route domain to (defaults to APPNAME-web)
	-c, --tls-cert=<tls-cert>  path to PEM encoded certificate for TLS, - for stdin (http only)
	-k, --tls-key=<tls-key>    path to PEM encoded private key for TLS, - for stdin (http only)
	--sticky                   enable cookie-based sticky routing (http only)
	--no-sticky                disable cookie-based sticky routing (update http only)
	-p, --port=<port>          port to accept traffic on (tcp only)

Commands:
	With no arguments, shows a list of routes.
	
	add     adds a route to an app
	remove  removes a route

Examples:

	$ flynn route add http example.com

	$ flynn route add tcp
`)
}

func runRoute(args *docopt.Args, client *controller.Client) error {
	if args.Bool["add"] {
		switch {
		case args.Bool["http"]:
			return runRouteAddHTTP(args, client)
		case args.Bool["tcp"]:
			return runRouteAddTCP(args, client)
		default:
			return fmt.Errorf("Route type %s not supported.", args.String["-t"])
		}
	} else if args.Bool["update"] {
		typ := strings.Split(args.String["<id>"], "/")[0]
		switch typ {
		case "http":
			return runRouteUpdateHTTP(args, client)
		case "tcp":
			return runRouteUpdateTCP(args, client)
		default:
			return fmt.Errorf("Route type %s not supported.", typ)
		}
	} else if args.Bool["remove"] {
		return runRouteRemove(args, client)
	}

	routes, err := client.RouteList(mustApp())
	if err != nil {
		return err
	}

	w := tabWriter()
	defer w.Flush()

	var route, protocol, service, sticky string
	listRec(w, "ROUTE", "SERVICE", "ID", "STICKY")
	for _, k := range routes {
		switch k.Type {
		case "tcp":
			protocol = "tcp"
			route = strconv.Itoa(k.TCPRoute().Port)
			service = k.TCPRoute().Service
		case "http":
			route = k.HTTPRoute().Domain
			service = k.TCPRoute().Service
			if k.HTTPRoute().TLSCert == "" {
				protocol = "http"
			} else {
				protocol = "https"
			}
			sticky = fmt.Sprintf("%t", k.Sticky)
		}
		listRec(w, protocol+":"+route, service, k.FormattedID(), sticky)
	}
	return nil
}

func runRouteAddTCP(args *docopt.Args, client *controller.Client) error {
	service := args.String["--service"]
	if service == "" {
		service = mustApp() + "-web"
	}

	port := 0
	if args.String["--port"] != "" {
		p, err := strconv.Atoi(args.String["--port"])
		if err != nil {
			return err
		}
		port = p
	}

	hr := &router.TCPRoute{Service: service, Port: port}
	r := hr.ToRoute()
	if err := client.CreateRoute(mustApp(), r); err != nil {
		return err
	}
	hr = r.TCPRoute()
	fmt.Printf("%s listening on port %d\n", hr.FormattedID(), hr.Port)
	return nil
}

func runRouteAddHTTP(args *docopt.Args, client *controller.Client) error {
	service := args.String["--service"]
	if service == "" {
		service = mustApp() + "-web"
	}

	tlsCert, tlsKey, err := parseTLSCert(args)
	if err != nil {
		return err
	}

	hr := &router.HTTPRoute{
		Service: service,
		Domain:  args.String["<domain>"],
		TLSCert: tlsCert,
		TLSKey:  tlsKey,
		Sticky:  args.Bool["--sticky"],
	}
	route := hr.ToRoute()
	if err := client.CreateRoute(mustApp(), route); err != nil {
		return err
	}
	fmt.Println(route.FormattedID())
	return nil
}

func runRouteUpdateTCP(args *docopt.Args, client *controller.Client) error {
	id := args.String["<id>"]
	appName := mustApp()

	route, err := client.GetRoute(appName, id)
	if err != nil {
		return err
	}

	service := args.String["--service"]
	if service == "" {
		return errors.New("No service name given")
	}
	if service == route.Service {
		return errors.New("Service already set")
	}
	route.Service = service

	if err := client.UpdateRoute(appName, id, route); err != nil {
		return err
	}
	hr := route.TCPRoute()
	fmt.Printf("%s listening on port %d\n", hr.FormattedID(), hr.Port)
	return nil
}

func runRouteUpdateHTTP(args *docopt.Args, client *controller.Client) error {
	id := args.String["<id>"]
	appName := mustApp()

	route, err := client.GetRoute(appName, id)
	if err != nil {
		return err
	}

	if service := args.String["--service"]; service != "" {
		route.Service = service
	}

	route.TLSCert, route.TLSKey, err = parseTLSCert(args)
	if err != nil {
		return err
	}

	if args.Bool["--sticky"] {
		route.Sticky = true
	} else if args.Bool["--no-sticky"] {
		route.Sticky = false
	}

	if err := client.UpdateRoute(appName, id, route); err != nil {
		return err
	}
	fmt.Printf("updated %s\n", route.FormattedID())
	return nil
}

func parseTLSCert(args *docopt.Args) (string, string, error) {
	tlsCertPath := args.String["--tls-cert"]
	tlsKeyPath := args.String["--tls-key"]
	var tlsCert []byte
	var tlsKey []byte
	if tlsCertPath != "" && tlsKeyPath != "" {
		var stdin []byte

		if tlsCertPath == "-" || tlsKeyPath == "-" {
			var err error
			stdin, err = ioutil.ReadAll(os.Stdin)
			if err != nil {
				return "", "", fmt.Errorf("Failed to read from stdin: %s", err)
			}
		}

		var err error
		tlsCert, err = readPEM("CERTIFICATE", tlsCertPath, stdin)
		if err != nil {
			return "", "", fmt.Errorf("Failed to read TLS cert: %s", err)
		}
		tlsKey, err = readPEM("PRIVATE KEY", tlsKeyPath, stdin)
		if err != nil {
			return "", "", fmt.Errorf("Failed to read TLS key: %s", err)
		}
	} else if tlsCertPath != "" || tlsKeyPath != "" {
		return "", "", errors.New("Both the TLS certificate AND private key need to be specified")
	}
	return string(tlsCert), string(tlsKey), nil
}

func readPEM(typ string, path string, stdin []byte) ([]byte, error) {
	if path == "-" {
		var buf bytes.Buffer
		var block *pem.Block
		for {
			block, stdin = pem.Decode(stdin)
			if block == nil {
				break
			}
			if block.Type == typ {
				pem.Encode(&buf, block)
			}
		}
		if buf.Len() > 0 {
			return buf.Bytes(), nil
		}
		return nil, errors.New("No PEM blocks found in stdin")
	}
	return ioutil.ReadFile(path)
}

func runRouteRemove(args *docopt.Args, client *controller.Client) error {
	routeID := args.String["<id>"]

	if err := client.DeleteRoute(mustApp(), routeID); err != nil {
		return err
	}
	fmt.Printf("Route %s removed.\n", routeID)
	return nil
}
