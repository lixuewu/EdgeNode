// Copyright 2021 Liuxiangchao iwind.liu@gmail.com. All rights reserved.

package nodes

import (
	"errors"
	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/firewallconfigs"
	"github.com/TeaOSLab/EdgeNode/internal/conns"
	teaconst "github.com/TeaOSLab/EdgeNode/internal/const"
	"github.com/TeaOSLab/EdgeNode/internal/iplibrary"
	"github.com/TeaOSLab/EdgeNode/internal/stats"
	"github.com/TeaOSLab/EdgeNode/internal/ttlcache"
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"github.com/TeaOSLab/EdgeNode/internal/waf"
	"github.com/iwind/TeaGo/Tea"
	"github.com/iwind/TeaGo/types"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ClientConn 客户端连接
type ClientConn struct {
	BaseClientConn

	createdAt int64

	isTLS   bool
	isHTTP  bool
	hasRead bool

	isLO          bool // 是否为环路
	isInAllowList bool

	hasResetSYNFlood bool

	lastReadAt  int64
	lastWriteAt int64
	lastErr     error

	readDeadlineTime int64
	isShortReading   bool // reading header or tls handshake

	isDebugging      bool
	autoReadTimeout  bool
	autoWriteTimeout bool
}

func NewClientConn(rawConn net.Conn, isHTTP bool, isTLS bool, isInAllowList bool) net.Conn {
	// 是否为环路
	var remoteAddr = rawConn.RemoteAddr().String()
	var isLO = strings.HasPrefix(remoteAddr, "127.0.0.1:") || strings.HasPrefix(remoteAddr, "[::1]:")

	var conn = &ClientConn{
		BaseClientConn: BaseClientConn{rawConn: rawConn},
		isTLS:          isTLS,
		isHTTP:         isHTTP,
		isLO:           isLO,
		isInAllowList:  isInAllowList,
		createdAt:      time.Now().Unix(),
	}

	var globalServerConfig = sharedNodeConfig.GlobalServerConfig
	if globalServerConfig != nil {
		var performanceConfig = globalServerConfig.Performance
		conn.isDebugging = performanceConfig.Debug
		conn.autoReadTimeout = performanceConfig.AutoReadTimeout
		conn.autoWriteTimeout = performanceConfig.AutoWriteTimeout
	}

	if isHTTP {
		// TODO 可以在配置中设置此值
		_ = conn.SetLinger(nodeconfigs.DefaultTCPLinger)
	}

	// 加入到Map
	conns.SharedMap.Add(conn)

	return conn
}

func (this *ClientConn) Read(b []byte) (n int, err error) {
	if this.isDebugging {
		this.lastReadAt = time.Now().Unix()

		defer func() {
			if err != nil {
				this.lastErr = errors.New("read error: " + err.Error())
			} else {
				this.lastErr = nil
			}
		}()
	}

	// 环路直接读取
	if this.isLO {
		n, err = this.rawConn.Read(b)
		if n > 0 {
			atomic.AddUint64(&teaconst.InTrafficBytes, uint64(n))
		}
		return
	}

	// 设置读超时时间
	if this.isHTTP && !this.isWebsocket && !this.isShortReading && this.autoReadTimeout {
		this.setHTTPReadTimeout()
	}

	// 开始读取
	n, err = this.rawConn.Read(b)
	if n > 0 {
		atomic.AddUint64(&teaconst.InTrafficBytes, uint64(n))
		this.hasRead = true
	}

	// 检测是否为超时错误
	var isTimeout = err != nil && os.IsTimeout(err)
	var isHandshakeError = isTimeout && !this.hasRead
	if isTimeout {
		_ = this.SetLinger(0)
	} else {
		_ = this.SetLinger(nodeconfigs.DefaultTCPLinger)
	}

	// 忽略白名单和局域网
	if this.isHTTP && !this.isInAllowList && !utils.IsLocalIP(this.RawIP()) {
		// SYN Flood检测
		if this.serverId == 0 || !this.hasResetSYNFlood {
			var synFloodConfig = sharedNodeConfig.SYNFloodConfig()
			if synFloodConfig != nil && synFloodConfig.IsOn {
				if isHandshakeError {
					this.increaseSYNFlood(synFloodConfig)
				} else if err == nil && !this.hasResetSYNFlood {
					this.hasResetSYNFlood = true
					this.resetSYNFlood()
				}
			}
		}
	}

	return
}

func (this *ClientConn) Write(b []byte) (n int, err error) {
	if this.isDebugging {
		this.lastWriteAt = time.Now().Unix()

		defer func() {
			if err != nil {
				this.lastErr = errors.New("write error: " + err.Error())
			} else {
				this.lastErr = nil
			}
		}()
	}

	// 设置写超时时间
	if this.autoWriteTimeout {
		// TODO L2 -> L1 写入时不限制时间
		var timeoutSeconds = len(b) / 1024
		if timeoutSeconds < 3 {
			timeoutSeconds = 3
		}
		_ = this.rawConn.SetWriteDeadline(time.Now().Add(time.Duration(timeoutSeconds) * time.Second)) // TODO 时间可以设置
	}

	// 延长读超时时间
	if this.isHTTP && !this.isWebsocket && this.autoReadTimeout {
		this.setHTTPReadTimeout()
	}

	// 开始写入
	n, err = this.rawConn.Write(b)
	if n > 0 {
		// 统计当前服务带宽
		if this.serverId > 0 {
			if !this.isLO || Tea.IsTesting() { // 环路不统计带宽，避免缓存预热等行为产生带宽
				atomic.AddUint64(&teaconst.OutTrafficBytes, uint64(n))
				stats.SharedBandwidthStatManager.Add(this.userId, this.serverId, int64(n))
			}
		}
	}

	// 如果是写入超时，则立即关闭连接
	if err != nil && os.IsTimeout(err) {
		// TODO 考虑对多次慢连接的IP做出惩罚
		conn, ok := this.rawConn.(LingerConn)
		if ok {
			_ = conn.SetLinger(0)
		}

		_ = this.Close()
	}

	return
}

func (this *ClientConn) Close() error {
	this.isClosed = true

	err := this.rawConn.Close()

	// 单个服务并发数限制
	// 不能加条件限制，因为服务配置随时有变化
	sharedClientConnLimiter.Remove(this.rawConn.RemoteAddr().String())

	// 从conn map中移除
	conns.SharedMap.Remove(this)

	return err
}

func (this *ClientConn) LocalAddr() net.Addr {
	return this.rawConn.LocalAddr()
}

func (this *ClientConn) RemoteAddr() net.Addr {
	return this.rawConn.RemoteAddr()
}

func (this *ClientConn) SetDeadline(t time.Time) error {
	return this.rawConn.SetDeadline(t)
}

func (this *ClientConn) SetReadDeadline(t time.Time) error {
	// 如果开启了HTTP自动读超时选项，则自动控制超时时间
	if this.isHTTP && !this.isWebsocket && this.autoReadTimeout {
		this.isShortReading = false

		var unixTime = t.Unix()
		if unixTime < 10 {
			return nil
		}
		if unixTime == this.readDeadlineTime {
			return nil
		}
		this.readDeadlineTime = unixTime
		var seconds = -time.Since(t)
		if seconds <= 0 || seconds > HTTPIdleTimeout {
			return nil
		}
		if seconds < HTTPIdleTimeout-1*time.Second {
			this.isShortReading = true
		}
	}
	return this.rawConn.SetReadDeadline(t)
}

func (this *ClientConn) SetWriteDeadline(t time.Time) error {
	return this.rawConn.SetWriteDeadline(t)
}

func (this *ClientConn) CreatedAt() int64 {
	return this.createdAt
}

func (this *ClientConn) LastReadAt() int64 {
	return this.lastReadAt
}

func (this *ClientConn) LastWriteAt() int64 {
	return this.lastWriteAt
}

func (this *ClientConn) LastErr() error {
	return this.lastErr
}

func (this *ClientConn) resetSYNFlood() {
	ttlcache.SharedCache.Delete("SYN_FLOOD:" + this.RawIP())
}

func (this *ClientConn) increaseSYNFlood(synFloodConfig *firewallconfigs.SYNFloodConfig) {
	var ip = this.RawIP()
	if len(ip) > 0 && !iplibrary.IsInWhiteList(ip) && (!synFloodConfig.IgnoreLocal || !utils.IsLocalIP(ip)) {
		var timestamp = utils.NextMinuteUnixTime()
		var result = ttlcache.SharedCache.IncreaseInt64("SYN_FLOOD:"+ip, 1, timestamp, true)
		var minAttempts = synFloodConfig.MinAttempts
		if minAttempts < 5 {
			minAttempts = 5
		}
		if !this.isTLS {
			// 非TLS，设置为两倍，防止误封
			minAttempts = 2 * minAttempts
		}
		if result >= int64(minAttempts) {
			var timeout = synFloodConfig.TimeoutSeconds
			if timeout <= 0 {
				timeout = 600
			}

			// 关闭当前连接
			_ = this.SetLinger(0)
			_ = this.Close()

			waf.SharedIPBlackList.RecordIP(waf.IPTypeAll, firewallconfigs.FirewallScopeGlobal, 0, ip, time.Now().Unix()+int64(timeout), 0, true, 0, 0, "疑似SYN Flood攻击，当前1分钟"+types.String(result)+"次空连接")
		}
	}
}

// 设置读超时时间
func (this *ClientConn) setHTTPReadTimeout() {
	_ = this.SetReadDeadline(time.Now().Add(HTTPIdleTimeout))
}
