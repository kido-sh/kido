// Command netprobe is the guest side of test/vz-network.sh:
//
//	netprobe dial HOST:PORT [TIMEOUT]      connect over TCP, print the first line the peer sends
//	netprobe serve PORT [TEXT]             accept TCP connections forever, send TEXT and close
//	netprobe lookup NAME [TIMEOUT]         resolve NAME
//	netprobe udpserve PORT                 echo UDP datagrams forever, prefixed with "echo:"
//	netprobe udp HOST:PORT TEXT [TIMEOUT]  send one datagram, print the reply
//
// A network error exits 1 when it is a timeout (the packets were dropped) and 2 otherwise (for
// example refused: the peer answered).
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

// fail reports err and exits 1 for a timeout, 2 for any other error.
func fail(err error, f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		os.Exit(1)
	}
	os.Exit(2)
}

func timeout(i int) time.Duration {
	if len(os.Args) > i {
		if d, err := time.ParseDuration(os.Args[i]); err == nil {
			return d
		}
	}
	return 3 * time.Second
}

func main() {
	if len(os.Args) < 3 {
		die("usage: netprobe dial|serve|lookup|udpserve|udp ...")
	}
	switch os.Args[1] {
	case "dial":
		start := time.Now()
		c, err := net.DialTimeout("tcp", os.Args[2], timeout(3))
		if err != nil {
			fail(err, "dial %s: %v (%.0f ms)", os.Args[2], err, time.Since(start).Seconds()*1000)
		}
		c.SetReadDeadline(time.Now().Add(timeout(3)))
		line, _ := bufio.NewReader(c).ReadString('\n')
		fmt.Printf("connected %s -> %s in %.1f ms: %q\n", c.LocalAddr(), c.RemoteAddr(), time.Since(start).Seconds()*1000, line)
	case "serve":
		text := "hello"
		if len(os.Args) > 3 {
			text = os.Args[3]
		}
		l, err := net.Listen("tcp", ":"+os.Args[2])
		if err != nil {
			die("listen: %v", err)
		}
		for {
			c, err := l.Accept()
			if err != nil {
				continue
			}
			fmt.Fprintf(c, "%s from %s to %s\n", text, c.RemoteAddr(), c.LocalAddr())
			c.Close()
		}
	case "lookup":
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), timeout(3))
		defer cancel()
		addrs, err := net.DefaultResolver.LookupHost(ctx, os.Args[2])
		if err != nil {
			fail(err, "lookup %s: %v", os.Args[2], err)
		}
		fmt.Printf("%s -> %v in %.1f ms\n", os.Args[2], addrs, time.Since(start).Seconds()*1000)
	case "udpserve":
		pc, err := net.ListenPacket("udp", ":"+os.Args[2])
		if err != nil {
			die("listen: %v", err)
		}
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				continue
			}
			pc.WriteTo(append([]byte(fmt.Sprintf("echo from %s to %s: ", from, pc.LocalAddr())), buf[:n]...), from)
		}
	case "udp":
		c, err := net.Dial("udp", os.Args[2])
		if err != nil {
			die("dial: %v", err)
		}
		c.Write([]byte(os.Args[3]))
		c.SetReadDeadline(time.Now().Add(timeout(4)))
		buf := make([]byte, 2048)
		n, err := c.Read(buf)
		if err != nil {
			fail(err, "udp %s: %v", os.Args[2], err)
		}
		fmt.Printf("%s\n", buf[:n])
	default:
		die("unknown command %s", os.Args[1])
	}
}
