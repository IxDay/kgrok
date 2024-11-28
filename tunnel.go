package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mdobak/go-xerrors"
	"github.com/omrikiei/ktunnel/pkg/common"
	pb "github.com/omrikiei/ktunnel/tunnel_pb"
	"google.golang.org/grpc"
)

type ForwardedPort struct {
	Local  uint16
	Remote uint16
}

// https://github.com/kubernetes/client-go/blob/37045084c2aa82927b0e5ffc752861430fd7e4ab/tools/portforward/portforward.go#L85
func ParsePorts(ports []string) ([]ForwardedPort, error) {
	var forwards []ForwardedPort
	for _, portString := range ports {
		parts := strings.Split(portString, ":")
		var localString, remoteString string
		if len(parts) == 1 {
			localString = parts[0]
			remoteString = parts[0]
		} else if len(parts) == 2 {
			localString = parts[0]
			if localString == "" {
				// support :5000
				localString = "0"
			}
			remoteString = parts[1]
		} else {
			return nil, fmt.Errorf("invalid port format '%s'", portString)
		}

		localPort, err := strconv.ParseUint(localString, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("error parsing local port '%s': %s", localString, err)
		}

		remotePort, err := strconv.ParseUint(remoteString, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("error parsing remote port '%s': %s", remoteString, err)
		}
		if remotePort == 0 {
			return nil, fmt.Errorf("remote port must be > 0")
		}

		forwards = append(forwards, ForwardedPort{uint16(localPort), uint16(remotePort)})
	}

	return forwards, nil
}

func Tunnel(ctx context.Context, port int, fps []ForwardedPort) error {
	wg, m := sync.WaitGroup{}, sync.Mutex{}
	var errs error

	ctx, cancel := context.WithCancelCause(ctx)
	conn, err := grpc.Dial(
		fmt.Sprintf("localhost:%d", port),
		grpc.WithInsecure(),
	)
	if err != nil {
		return xerrors.New("failed to dial", err)
	}
	defer conn.Close()

	client := pb.NewTunnelClient(conn)
	for _, fp := range fps {
		fmt.Printf("starting tunnel from remote %d to local %d\n", fp.Remote, fp.Local)

		req := &pb.SocketDataRequest{
			Port:     int32(fp.Remote),
			LogLevel: 0,
			Scheme:   pb.TunnelScheme(pb.TunnelScheme_value["TCP"]),
		}

		stream, err := client.InitTunnel(ctx)
		if err != nil {
			cancel(xerrors.New("failed initializing tunnel", err))
			break
		} else if err := stream.Send(req); err != nil {
			cancel(xerrors.New("failed sending initial tunnel request", err))
			break
		}

		sessions := make(chan *common.Session)
		wg.Add(2)
		go func() {
			ReceiveData(stream, sessions, fp.Local)
			wg.Done()
		}()
		go func() {
			err := SendData(stream, sessions)
			m.Lock()
			errs = xerrors.Append(errs, err)
			m.Unlock()
			wg.Done()
		}()
	}
	wg.Wait()

	return xerrors.WithWrapper(context.Cause(ctx), errs)
}

func ReadFromSession(
	session *common.Session,
	sessionsOut chan<- *common.Session,
) {
	fmt.Printf("session=%s started reading conn\n", session.Id)
	defer fmt.Printf("session=%s finished reading conn\n", session.Id)

	conn := session.Conn
	buff := make([]byte, common.BufferSize)
	for {
		br, err := conn.Read(buff)
		if err != nil {
			if err != io.EOF {
				fmt.Printf(
					"session=%s failed reading from socket: %s\n",
					session.Id,
					err,
				)
			} else {
				fmt.Printf("session=%s got EOF from connection\n", session.Id)
			}

			session.Open = false
			sessionsOut <- session
			return
		}
		select {
		case <-session.Context.Done():
			return
		default:
			fmt.Printf("session=%s read %d bytes from conn\n", session.Id, br)
			session.Lock()
			if br > 0 {
				fmt.Printf(
					"session=%s wrote %d bytes to session buf\n",
					session.Id,
					br,
				)
				_, err = session.Buf.Write(buff[0:br])
			}
			session.Unlock()

			if err != nil {
				fmt.Printf(
					"session=%s failed writing to session buffer: %s\n",
					session.Id,
					err,
				)
				return
			}
			sessionsOut <- session
		}

	}
}

func ReceiveData(
	st pb.Tunnel_InitTunnelClient,
	sessionsOut chan<- *common.Session,
	port uint16,
) error {
	for {
		fmt.Println("attempting to receive from stream")
		m, err := st.Recv()
		if err != nil {
			return xerrors.New("error reading from stream", err)
		}
		select {
		case <-st.Context().Done():
			fmt.Printf("closing listener on :%d", port)
			if err := st.Context().Err(); errors.Is(err, context.Canceled) ||
				err == nil {
				fmt.Printf("\n")
				return st.CloseSend()
			} else {
				fmt.Printf("with error: %s\n", err)
				return xerrors.WithWrapper(err, st.CloseSend())
			}
		default:
			requestId, err := uuid.Parse(m.RequestId)
			if err != nil {
				fmt.Printf(
					"session=%s failed parsing session uuid from stream: %s, skipping\n",
					m.RequestId,
					err,
				)
			}

			session, exists := common.GetSession(requestId)
			if exists == false {
				fmt.Printf("session=%s new connection %d\n", m.RequestId, port)

				// new session
				timeout := time.Millisecond * 500
				conn, err := net.DialTimeout("tcp", fmt.Sprintf(":%d", port), timeout)
				if err != nil {
					fmt.Printf("failed connecting to local port %d\n", port)
					// close the remote connection
					resp := &pb.SocketDataRequest{
						RequestId:   requestId.String(),
						ShouldClose: true,
					}
					err := st.Send(resp)
					if err != nil {
						fmt.Printf(
							"failed sending close message to tunnel stream\n",
						)
					}

					continue
				} else {
					session = common.NewSessionFromStream(requestId, conn)
					go ReadFromSession(session, sessionsOut)
				}
			} else if m.ShouldClose {
				session.Open = false
			}

			// process the data from the server
			handleStreamData(m, session)
		}
	}
}

func SendData(
	stream pb.Tunnel_InitTunnelClient,
	sessions <-chan *common.Session,
) error {
	for {
		select {
		case <-stream.Context().Done():
			err := stream.Context().Err()
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case session := <-sessions:
			// read the bytes from the buffer
			// but allow it to keep growing while we send the response
			session.Lock()
			bys := session.Buf.Len()
			bytes := make([]byte, bys)
			_, err := session.Buf.Read(bytes)
			if err != nil {
				fmt.Printf(
					"failed reading stream from session %v, exiting\n",
					err,
				)
				return err
			}

			fmt.Printf(
				"session=%s read %d from buffer out of %d available\n",
				session.Id,
				len(bytes),
				bys,
			)

			resp := &pb.SocketDataRequest{
				RequestId:   session.Id.String(),
				Data:        bytes,
				ShouldClose: !session.Open,
			}
			session.Unlock()

			fmt.Printf(
				"session=%s sending %d bytes to server\n",
				session.Id,
				len(bytes),
			)
			err = stream.Send(resp)
			if err != nil {
				fmt.Printf(
					"failed sending message to tunnel stream: %s, exiting\n",
					err,
				)
				return err
			}
			fmt.Printf(
				"session=%s sent %d bytes to server\n",
				session.Id,
				len(bytes),
			)
		}
	}
}

func handleStreamData(m *pb.SocketDataResponse, session *common.Session) {
	if session.Open == false {
		fmt.Printf("session=%s closed session\n", session.Id)
		session.Close()
		return
	}

	data := m.GetData()
	fmt.Printf(
		"session=%s received %d bytes from server\n",
		session.Id,
		len(data),
	)
	if len(data) > 0 {
		session.Lock()
		fmt.Printf("session=%s wrote %d bytes to conn\n", session.Id, len(data))
		_, err := session.Conn.Write(data)
		session.Unlock()
		if err != nil {
			fmt.Printf(
				"session=%s failed writing to socket: %s, closing session\n",
				session.Id,
				err,
			)
			session.Close()
			return
		}
	}
}
