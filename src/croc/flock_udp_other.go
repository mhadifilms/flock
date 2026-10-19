//go:build !darwin

package croc

import "net"

func flockPacketConn(socket *net.UDPConn) net.PacketConn { return socket }
