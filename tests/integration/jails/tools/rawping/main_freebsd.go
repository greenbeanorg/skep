//go:build skep_testtasks && freebsd

package main

import (
	"flag"
	"fmt"
	"net/netip"
	"os"
	"syscall"
	"time"
)

func main() {
	srcS := flag.String("src", "", "IPv4 source address to put in the header (may be forged)")
	dstS := flag.String("dst", "", "IPv4 destination address")
	count := flag.Int("count", 1, "echo requests to send, one per second")
	ttl := flag.Int("ttl", 64, "IP TTL")
	id := flag.Int("id", 0x5302, "ICMP identifier and IP ID")
	flag.Parse()
	src, err1 := netip.ParseAddr(*srcS)
	dst, err2 := netip.ParseAddr(*dstS)
	if err1 != nil || err2 != nil || *count < 1 || *ttl < 1 || *ttl > 255 {
		fmt.Fprintln(os.Stderr, "usage: rawping -src A.B.C.D -dst A.B.C.D [-count N] [-ttl N] [-id N]")
		os.Exit(2)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rawping: socket(AF_INET, SOCK_RAW, IPPROTO_RAW): %v\n", err)
		os.Exit(1)
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_HDRINCL, 1); err != nil {
		fmt.Fprintf(os.Stderr, "rawping: setsockopt(IP_HDRINCL): %v\n", err)
		os.Exit(1)
	}
	to := &syscall.SockaddrInet4{Addr: dst.As4()}
	failed := 0
	for seq := 0; seq < *count; seq++ {
		if seq > 0 {
			time.Sleep(time.Second)
		}
		pkt, err := echoPacket(src, dst, uint8(*ttl), uint16(*id), uint16(seq), []byte("skep-sr02"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		now := time.Now().UTC().Format("15:04:05.000")
		if err := syscall.Sendto(fd, pkt, 0, to); err != nil {
			fmt.Printf("%sZ seq %d %v -> %v: sendto failed: %v\n", now, seq, src, dst, err)
			failed++
			continue
		}
		fmt.Printf("%sZ seq %d %v -> %v: sent %d bytes\n", now, seq, src, dst, len(pkt))
	}
	if failed > 0 {
		os.Exit(1)
	}
}
