package main

/*
#include <stdlib.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/uio.h>

#define ANCIL_FD_BUFFER(n) \
    struct { \
	struct cmsghdr h; \
	int fd[n]; \
    }

int
ancil_send_fds_with_buffer(int sock, const int *fds, unsigned n_fds, void *buffer)
{
    struct msghdr msghdr;
    char nothing = '!';
    struct iovec nothing_ptr;
    struct cmsghdr *cmsg;
    int i;

    nothing_ptr.iov_base = &nothing;
    nothing_ptr.iov_len = 1;
    msghdr.msg_name = NULL;
    msghdr.msg_namelen = 0;
    msghdr.msg_iov = &nothing_ptr;
    msghdr.msg_iovlen = 1;
    msghdr.msg_flags = 0;
    msghdr.msg_control = buffer;
    msghdr.msg_controllen = sizeof(struct cmsghdr) + sizeof(int) * n_fds;
    cmsg = CMSG_FIRSTHDR(&msghdr);
    cmsg->cmsg_len = msghdr.msg_controllen;
    cmsg->cmsg_level = SOL_SOCKET;
    cmsg->cmsg_type = SCM_RIGHTS;
    for(i = 0; i < n_fds; i++)
	((int *)CMSG_DATA(cmsg))[i] = fds[i];
    return(sendmsg(sock, &msghdr, 0) >= 0 ? 0 : -1);
}

int
ancil_send_fd(int sock, int fd)
{
    ANCIL_FD_BUFFER(1) buffer;

    return(ancil_send_fds_with_buffer(sock, &fd, 1, &buffer));
}

*/
import "C"

import (
	"crypto/sha1"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
    "syscall"
	"runtime"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/golang/snappy"
	"github.com/hashicorp/yamux"
	"github.com/urfave/cli"
	kcp "github.com/xtaci/kcp-go"
)

var (
	// VERSION is injected by buildflags
	VERSION = "SELFBUILD"
	// SALT is use for pbkdf2 key expansion
	SALT = "kcp-go"
)

type compStream struct {
	conn net.Conn
	w    *snappy.Writer
	r    *snappy.Reader
}

func (c *compStream) Read(p []byte) (n int, err error) {
	return c.r.Read(p)
}

func (c *compStream) Write(p []byte) (n int, err error) {
	n, err = c.w.Write(p)
	err = c.w.Flush()
	return n, err
}

func (c *compStream) Close() error {
	return c.conn.Close()
}

func newCompStream(conn net.Conn) *compStream {
	c := new(compStream)
	c.conn = conn
	c.w = snappy.NewBufferedWriter(conn)
	c.r = snappy.NewReader(conn)
	return c
}

func gcTask(interval int) {
	if interval > 0 {
		ticker := time.NewTicker(time.Duration(interval) * time.Minute)
		defer ticker.Stop()
		for {
			<-ticker.C
			runtime.GC()
		}
	}
}

func handleClient(p1, p2 io.ReadWriteCloser) {
	log.Println("stream opened")
	defer log.Println("stream closed")
	defer p1.Close()
	defer p2.Close()

	// start tunnel
	p1die := make(chan struct{})
	go func() {
		io.Copy(p1, p2)
		close(p1die)
	}()

	p2die := make(chan struct{})
	go func() {
		io.Copy(p2, p1)
		close(p2die)
	}()

	// wait for tunnel termination
	select {
	case <-p1die:
	case <-p2die:
	}
}

func checkError(err error) {
	if err != nil {
		log.Println(err)
		os.Exit(-1)
	}
}

func main() {
	rand.Seed(int64(time.Now().Nanosecond()))
	if VERSION == "SELFBUILD" {
		// add more log flags for debugging
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}
	myApp := cli.NewApp()
	myApp.Name = "kcptun"
	myApp.Usage = "kcptun client"
	myApp.Version = VERSION
	myApp.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "localaddr,l",
			Value: ":12948",
			Usage: "local listen address",
		},
		cli.StringFlag{
			Name:  "remoteaddr, r",
			Value: "vps:29900",
			Usage: "kcp server address",
		},
		cli.StringFlag{
			Name:   "key",
			Value:  "it's a secrect",
			Usage:  "key for communcation, must be the same as kcptun server",
			EnvVar: "KCPTUN_KEY",
		},
		cli.StringFlag{
			Name:  "crypt",
			Value: "aes",
			Usage: "methods for encryption: aes, tea, xor, none",
		},
		cli.StringFlag{
			Name:  "mode",
			Value: "fast",
			Usage: "mode for communication: fast3, fast2, fast, normal",
		},
		cli.StringFlag{
			Name:  "path",
			Value: "/data/data/com.github.shadowsocks/protect_path",
			Usage: "path to shadowsocks data dir",
		},
		cli.IntFlag{
			Name:  "conn",
			Value: 1,
			Usage: "establish N physical connections as specified by 'conn' to server",
		},
		cli.IntFlag{
			Name:  "mtu",
			Value: 1350,
			Usage: "set MTU of UDP packets, suggest 'tracepath' to discover path mtu",
		},
		cli.IntFlag{
			Name:  "sndwnd",
			Value: 128,
			Usage: "set send window size(num of packets)",
		},
		cli.IntFlag{
			Name:  "rcvwnd",
			Value: 1024,
			Usage: "set receive window size(num of packets)",
		},
		cli.BoolFlag{
			Name:  "nocomp",
			Usage: "disable compression",
		},
		cli.IntFlag{
			Name:  "datashard",
			Value: 10,
			Usage: "set reed-solomon erasure coding - datashard",
		},
		cli.IntFlag{
			Name:  "parityshard",
			Value: 3,
			Usage: "set reed-solomon erasure coding - parityshard",
		},
		cli.IntFlag{
			Name:  "gcinterval",
			Value: 0,
			Usage: "manual GC interval(in minutes), set 0 to turn off manual GC",
		},
		cli.BoolFlag{
			Name:   "acknodelay",
			Usage:  "flush ack immediately when a packet is received",
			Hidden: true,
		},
		cli.IntFlag{
			Name:  "dscp",
			Value: 0,
			Usage: "set DSCP(6bit)",
		},
		cli.IntFlag{
			Name:   "nodelay",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "interval",
			Value:  40,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "resend",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "nc",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "sockbuf",
			Value:  4194304, // socket buffer size in bytes
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "keepalive",
			Value:  10, // nat keepalive interval in seconds
			Hidden: true,
		},
	}
	myApp.Action = func(c *cli.Context) error {
		log.Println("version:", VERSION)
		addr, err := net.ResolveTCPAddr("tcp", c.String("localaddr"))
		checkError(err)
		listener, err := net.ListenTCP("tcp", addr)
		checkError(err)

		// kcp server
		nodelay, interval, resend, nc := c.Int("nodelay"), c.Int("interval"), c.Int("resend"), c.Int("nc")

		switch c.String("mode") {
		case "normal":
			nodelay, interval, resend, nc = 0, 30, 2, 1
		case "fast":
			nodelay, interval, resend, nc = 0, 20, 2, 1
		case "fast2":
			nodelay, interval, resend, nc = 1, 20, 2, 1
		case "fast3":
			nodelay, interval, resend, nc = 1, 10, 2, 1
		}

		crypt := c.String("crypt")
		pass := pbkdf2.Key([]byte(c.String("key")), []byte(SALT), 4096, 32, sha1.New)
		var block kcp.BlockCrypt
		switch c.String("crypt") {
		case "tea":
			block, _ = kcp.NewTEABlockCrypt(pass[:16])
		case "xor":
			block, _ = kcp.NewSimpleXORBlockCrypt(pass)
		case "none":
			block, _ = kcp.NewNoneBlockCrypt(pass)
		default:
			block, _ = kcp.NewAESBlockCrypt(pass)
		}

		remoteaddr := c.String("remoteaddr")
		datashard, parityshard := c.Int("datashard"), c.Int("parityshard")
		mtu, sndwnd, rcvwnd := c.Int("mtu"), c.Int("sndwnd"), c.Int("rcvwnd")
		nocomp, acknodelay := c.Bool("nocomp"), c.Bool("acknodelay")
		dscp, sockbuf, keepalive, conn := c.Int("dscp"), c.Int("sockbuf"), c.Int("keepalive"), c.Int("conn")
		gcinterval := c.Int("gcinterval")

		log.Println("listening on:", listener.Addr())
		log.Println("encryption:", crypt)
		log.Println("nodelay parameters:", nodelay, interval, resend, nc)
		log.Println("remote address:", remoteaddr)
		log.Println("sndwnd:", sndwnd, "rcvwnd:", rcvwnd)
		log.Println("compression:", !nocomp)
		log.Println("mtu:", mtu)
		log.Println("datashard:", datashard, "parityshard:", parityshard)
		log.Println("acknodelay:", acknodelay)
		log.Println("dscp:", dscp)
		log.Println("sockbuf:", sockbuf)
		log.Println("keepalive:", keepalive)
		log.Println("gcinterval:", gcinterval)
		log.Println("conn:", conn)

		path := c.String("path")
		net.Callback = func(fd int) {
			socket, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err != nil {
				log.Println(err)
				return
			}
			defer syscall.Close(socket)

			err = syscall.Connect(socket, &syscall.SockaddrUnix{Name: path})
			if err != nil {
				log.Println(err)
				return
			}

			C.ancil_send_fd(C.int(socket), C.int(fd))

			dummy := []byte{1}
			n, err := syscall.Read(socket, dummy)
			if err != nil {
				log.Println(err)
				return
			}
			if n != 1 {
				log.Println("Failed to protect fd: ", fd)
				return
			}
		}

		go gcTask(gcinterval)
		config := &yamux.Config{
			AcceptBacklog:          256,
			EnableKeepAlive:        true,
			KeepAliveInterval:      30 * time.Second,
			ConnectionWriteTimeout: 30 * time.Second,
			MaxStreamWindowSize:    uint32(sockbuf),
			LogOutput:              os.Stderr,
		}
		createConn := func() *yamux.Session {
			kcpconn, err := kcp.DialWithOptions(remoteaddr, block, datashard, parityshard)
			checkError(err)
			kcpconn.SetStreamMode(true)
			kcpconn.SetNoDelay(nodelay, interval, resend, nc)
			kcpconn.SetWindowSize(sndwnd, rcvwnd)
			kcpconn.SetMtu(mtu)
			kcpconn.SetACKNoDelay(acknodelay)
			kcpconn.SetKeepAlive(keepalive)

			if err := kcpconn.SetDSCP(dscp); err != nil {
				log.Println("SetDSCP:", err)
			}
			if err := kcpconn.SetReadBuffer(sockbuf); err != nil {
				log.Println("SetReadBuffer:", err)
			}
			if err := kcpconn.SetWriteBuffer(sockbuf); err != nil {
				log.Println("SetWriteBuffer:", err)
			}

			// stream multiplex
			var session *yamux.Session
			if nocomp {
				session, err = yamux.Client(kcpconn, config)
			} else {
				session, err = yamux.Client(newCompStream(kcpconn), config)
			}
			checkError(err)
			return session
		}

		numconn := uint16(conn)
		var muxes []*yamux.Session
		for i := uint16(0); i < numconn; i++ {
			muxes = append(muxes, createConn())
		}

		rr := uint16(0)
		for {
			p1, err := listener.AcceptTCP()
			checkError(err)

			var p2 net.Conn
			if mux := muxes[rr%numconn]; mux == nil {
				// mux cache miss, create one
				mux = createConn()
				p2, err = mux.Open()
				if err != nil {
					// new mux open failed, close
					log.Printf("yamux open failed: %s, close", err)
					mux.Close()
					p1.Close()
					continue
				}
				// new mux open succ, put into cache
				muxes[rr%numconn] = mux
			} else {
				// cache hit, try to open the mux in cache
				p2, err = mux.Open()
				if err != nil {
					// cached mux open failed, close it and create a new one
					log.Printf("yamux open failed: %s, retry", err)
					mux.Close()
					muxes[rr%numconn] = nil // clear cache
					mux = createConn()
					p2, err = mux.Open()
					if err != nil {
						// new mux open fail again, close
						log.Printf("yamux open failed again: %s, close", err)
						mux.Close()
						p1.Close()
						continue
					}
					// new mux open succ, put into cache
					muxes[rr%numconn] = mux
				}
			}
			go handleClient(p1, p2)
			rr++
		}
	}
	myApp.Run(os.Args)
}
