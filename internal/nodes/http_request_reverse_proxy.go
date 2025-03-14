package nodes

import (
	"context"
	"errors"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/shared"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"github.com/iwind/TeaGo/types"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 处理反向代理
func (this *HTTPRequest) doReverseProxy() {
	if this.reverseProxy == nil {
		return
	}

	var retries = 3

	var failedOriginIds []int64
	var failedLnNodeIds []int64

	for i := 0; i < retries; i++ {
		originId, lnNodeId, shouldRetry := this.doOriginRequest(failedOriginIds, failedLnNodeIds, i == 0, i == retries-1)
		if !shouldRetry {
			break
		}
		if originId > 0 {
			failedOriginIds = append(failedOriginIds, originId)
		}
		if lnNodeId > 0 {
			failedLnNodeIds = append(failedLnNodeIds, lnNodeId)
		}
	}
}

// 请求源站
func (this *HTTPRequest) doOriginRequest(failedOriginIds []int64, failedLnNodeIds []int64, isFirstTry bool, isLastRetry bool) (originId int64, lnNodeId int64, shouldRetry bool) {
	// 对URL的处理
	var stripPrefix = this.reverseProxy.StripPrefix
	var requestURI = this.reverseProxy.RequestURI
	var requestURIHasVariables = this.reverseProxy.RequestURIHasVariables()
	var oldURI = this.uri

	var requestHost = ""
	if this.reverseProxy.RequestHostType == serverconfigs.RequestHostTypeCustomized {
		requestHost = this.reverseProxy.RequestHost
	}
	var requestHostHasVariables = this.reverseProxy.RequestHostHasVariables()

	// 源站
	var requestCall = shared.NewRequestCall()
	requestCall.Request = this.RawReq
	requestCall.Formatter = this.Format
	requestCall.Domain = this.ReqHost

	var origin *serverconfigs.OriginConfig

	// 二级节点
	var hasMultipleLnNodes = false
	if this.cacheRef != nil {
		origin, lnNodeId, hasMultipleLnNodes = this.getLnOrigin(failedLnNodeIds)
		if origin != nil {
			// 强制变更原来访问的域名
			requestHost = this.ReqHost
		}

		// 回源Header中去除If-None-Match和If-Modified-Since
		if !this.cacheRef.EnableIfNoneMatch {
			this.DeleteHeader("If-None-Match")
		}
		if !this.cacheRef.EnableIfModifiedSince {
			this.DeleteHeader("If-Modified-Since")
		}
	}

	// 自定义源站
	if origin == nil {
		if !isFirstTry {
			origin = this.reverseProxy.AnyOrigin(requestCall, failedOriginIds)
		}
		if origin == nil {
			origin = this.reverseProxy.NextOrigin(requestCall)
		}
		requestCall.CallResponseCallbacks(this.writer)
		if origin == nil {
			err := errors.New(this.URL() + ": no available origin sites for reverse proxy")
			remotelogs.ServerError(this.ReqServer.Id, "HTTP_REQUEST_REVERSE_PROXY", err.Error(), "", nil)
			this.write50x(err, http.StatusBadGateway, "No origin site yet", "尚未配置源站", true)
			return
		}
		originId = origin.Id

		if len(origin.StripPrefix) > 0 {
			stripPrefix = origin.StripPrefix
		}
		if len(origin.RequestURI) > 0 {
			requestURI = origin.RequestURI
			requestURIHasVariables = origin.RequestURIHasVariables()
		}
	}

	this.origin = origin // 设置全局变量是为了日志等处理

	if len(origin.RequestHost) > 0 {
		requestHost = origin.RequestHost
		requestHostHasVariables = origin.RequestHostHasVariables()
	}

	// 处理Scheme
	if origin.Addr == nil {
		err := errors.New(this.URL() + ": Origin '" + strconv.FormatInt(origin.Id, 10) + "' does not has a address")
		remotelogs.ErrorServer("HTTP_REQUEST_REVERSE_PROXY", err.Error())
		this.write50x(err, http.StatusBadGateway, "Origin site did not has a valid address", "源站尚未配置地址", true)
		return
	}
	this.RawReq.URL.Scheme = origin.Addr.Protocol.Primary().Scheme()

	// StripPrefix
	if len(stripPrefix) > 0 {
		if stripPrefix[0] != '/' {
			stripPrefix = "/" + stripPrefix
		}
		this.uri = strings.TrimPrefix(this.uri, stripPrefix)
		if len(this.uri) == 0 || this.uri[0] != '/' {
			this.uri = "/" + this.uri
		}
	}

	// RequestURI
	if len(requestURI) > 0 {
		if requestURIHasVariables {
			this.uri = this.Format(requestURI)
		} else {
			this.uri = requestURI
		}
		if len(this.uri) == 0 || this.uri[0] != '/' {
			this.uri = "/" + this.uri
		}

		// 处理RequestURI中的问号
		var questionMark = strings.LastIndex(this.uri, "?")
		if questionMark > 0 {
			var path = this.uri[:questionMark]
			if strings.Contains(path, "?") {
				this.uri = path + "&" + this.uri[questionMark+1:]
			}
		}

		// 去除多个/
		this.uri = utils.CleanPath(this.uri)
	}

	// 获取源站地址
	var originAddr = origin.Addr.PickAddress()
	if origin.Addr.HostHasVariables() {
		originAddr = this.Format(originAddr)
	}

	// 端口跟随
	if origin.FollowPort {
		var originHostIndex = strings.Index(originAddr, ":")
		if originHostIndex < 0 {
			var originErr = errors.New(this.URL() + ": Invalid origin address '" + originAddr + "', lacking port")
			remotelogs.ErrorServer("HTTP_REQUEST_REVERSE_PROXY", originErr.Error())
			this.write50x(originErr, http.StatusBadGateway, "No port in origin site address", "源站地址中没有配置端口", true)
			return
		}
		originAddr = originAddr[:originHostIndex+1] + types.String(this.requestServerPort())
	}
	this.originAddr = originAddr

	// RequestHost
	if len(requestHost) > 0 {
		if requestHostHasVariables {
			this.RawReq.Host = this.Format(requestHost)
		} else {
			this.RawReq.Host = requestHost
		}

		// 是否移除端口
		if this.reverseProxy.RequestHostExcludingPort {
			this.RawReq.Host = utils.ParseAddrHost(this.RawReq.Host)
		}

		this.RawReq.URL.Host = this.RawReq.Host
	} else if this.reverseProxy.RequestHostType == serverconfigs.RequestHostTypeOrigin {
		// 源站主机名
		var hostname = originAddr
		if origin.Addr.Protocol.IsHTTPFamily() {
			hostname = strings.TrimSuffix(hostname, ":80")
		} else if origin.Addr.Protocol.IsHTTPSFamily() {
			hostname = strings.TrimSuffix(hostname, ":443")
		}

		this.RawReq.Host = hostname

		// 是否移除端口
		if this.reverseProxy.RequestHostExcludingPort {
			this.RawReq.Host = utils.ParseAddrHost(this.RawReq.Host)
		}

		this.RawReq.URL.Host = this.RawReq.Host
	} else {
		this.RawReq.URL.Host = this.ReqHost

		// 是否移除端口
		if this.reverseProxy.RequestHostExcludingPort {
			this.RawReq.Host = utils.ParseAddrHost(this.RawReq.Host)
			this.RawReq.URL.Host = utils.ParseAddrHost(this.RawReq.URL.Host)
		}
	}

	// 重组请求URL
	var questionMark = strings.Index(this.uri, "?")
	if questionMark > -1 {
		this.RawReq.URL.Path = this.uri[:questionMark]
		this.RawReq.URL.RawQuery = this.uri[questionMark+1:]
	} else {
		this.RawReq.URL.Path = this.uri
		this.RawReq.URL.RawQuery = ""
	}
	this.RawReq.RequestURI = ""

	// 处理Header
	this.setForwardHeaders(this.RawReq.Header)
	this.processRequestHeaders(this.RawReq.Header)

	// 调用回调
	this.onRequest()
	if this.writer.isFinished {
		return
	}

	// 判断是否为Websocket请求
	if this.RawReq.Header.Get("Upgrade") == "websocket" {
		shouldRetry = this.doWebsocket(requestHost, isLastRetry)
		return
	}

	// 获取请求客户端
	client, err := SharedHTTPClientPool.Client(this, origin, originAddr, this.reverseProxy.ProxyProtocol, this.reverseProxy.FollowRedirects)
	if err != nil {
		remotelogs.ErrorServer("HTTP_REQUEST_REVERSE_PROXY", this.URL()+": Create client failed: "+err.Error())
		this.write50x(err, http.StatusBadGateway, "Failed to create origin site client", "构造源站客户端失败", true)
		return
	}

	// 开始请求
	resp, err := client.Do(this.RawReq)
	if err != nil {
		// 客户端取消请求，则不提示
		httpErr, ok := err.(*url.Error)
		if !ok {
			SharedOriginStateManager.Fail(origin, requestHost, this.reverseProxy, func() {
				this.reverseProxy.ResetScheduling()
			})
			this.write50x(err, http.StatusBadGateway, "Failed to read origin site", "源站读取失败", true)
			remotelogs.WarnServer("HTTP_REQUEST_REVERSE_PROXY", this.RawReq.URL.String()+": Request origin server failed: "+err.Error())
		} else if httpErr.Err != context.Canceled {
			SharedOriginStateManager.Fail(origin, requestHost, this.reverseProxy, func() {
				this.reverseProxy.ResetScheduling()
			})

			// 是否需要重试
			if (originId > 0 || (lnNodeId > 0 && hasMultipleLnNodes)) && !isLastRetry {
				shouldRetry = true
				this.uri = oldURI // 恢复备份

				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}

				if httpErr.Err != io.EOF {
					remotelogs.WarnServer("HTTP_REQUEST_REVERSE_PROXY", this.URL()+": Request origin server failed: "+err.Error())
				}

				return
			}

			if httpErr.Timeout() {
				this.write50x(err, http.StatusGatewayTimeout, "Read origin site timeout", "源站读取超时", true)
			} else if httpErr.Temporary() {
				this.write50x(err, http.StatusServiceUnavailable, "Origin site unavailable now", "源站当前不可用", true)
			} else {
				this.write50x(err, http.StatusBadGateway, "Failed to read origin site", "源站读取失败", true)
			}
			if httpErr.Err != io.EOF {
				remotelogs.WarnServer("HTTP_REQUEST_REVERSE_PROXY", this.URL()+": Request origin server failed: "+err.Error())
			}
		} else {
			// 是否为客户端方面的错误
			var isClientError = false
			if ok {
				if httpErr.Err == context.Canceled {
					// 如果是服务器端主动关闭，则无需提示
					if this.isConnClosed() {
						this.disableLog = true
						return
					}

					isClientError = true
					this.addError(errors.New(httpErr.Op + " " + httpErr.URL + ": client closed the connection"))
					this.writer.WriteHeader(499) // 仿照nginx
				}
			}

			if !isClientError {
				this.write50x(err, http.StatusBadGateway, "Failed to read origin site", "源站读取失败", true)
			}
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return
	}

	// 记录相关数据
	this.originStatus = int32(resp.StatusCode)

	// 恢复源站状态
	if !origin.IsOk {
		SharedOriginStateManager.Success(origin, func() {
			this.reverseProxy.ResetScheduling()
		})
	}

	// WAF对出站进行检查
	if this.web.FirewallRef != nil && this.web.FirewallRef.IsOn {
		if this.doWAFResponse(resp) {
			err = resp.Body.Close()
			if err != nil {
				remotelogs.WarnServer("HTTP_REQUEST_REVERSE_PROXY", this.URL()+": Closing Error (WAF): "+err.Error())
			}
			return
		}
	}

	// 特殊页面
	if len(this.web.Pages) > 0 && this.doPage(resp.StatusCode) {
		err = resp.Body.Close()
		if err != nil {
			remotelogs.WarnServer("HTTP_REQUEST_REVERSE_PROXY", this.URL()+": Closing error (Page): "+err.Error())
		}
		return
	}

	// 设置Charset
	// TODO 这里应该可以设置文本类型的列表，以及是否强制覆盖所有文本类型的字符集
	if this.web.Charset != nil && this.web.Charset.IsOn && len(this.web.Charset.Charset) > 0 {
		contentTypes, ok := resp.Header["Content-Type"]
		if ok && len(contentTypes) > 0 {
			var contentType = contentTypes[0]
			if _, found := textMimeMap[contentType]; found {
				resp.Header["Content-Type"][0] = contentType + "; charset=" + this.web.Charset.Charset
			}
		}
	}

	// 替换Location中的源站地址
	var locationHeader = resp.Header.Get("Location")
	if len(locationHeader) > 0 {
		// 空Location处理
		if locationHeader == emptyHTTPLocation {
			resp.Header.Del("Location")
		} else {
			// 自动修正Location中的源站地址
			locationURL, err := url.Parse(locationHeader)
			if err == nil && locationURL.Host != this.ReqHost && (locationURL.Host == originAddr || strings.HasPrefix(originAddr, locationURL.Host+":")) {
				locationURL.Host = this.ReqHost

				var oldScheme = locationURL.Scheme

				// 尝试和当前Scheme一致
				if this.IsHTTP {
					locationURL.Scheme = "http"
				} else if this.IsHTTPS {
					locationURL.Scheme = "https"
				}

				// 如果和当前URL一样，则可能是http -> https，防止无限循环
				if locationURL.String() == this.URL() {
					locationURL.Scheme = oldScheme
					resp.Header.Set("Location", locationURL.String())
				} else {
					resp.Header.Set("Location", locationURL.String())
				}
			}
		}
	}

	// 响应Header
	this.writer.AddHeaders(resp.Header)
	this.processResponseHeaders(this.writer.Header(), resp.StatusCode)

	// 是否需要刷新
	var shouldAutoFlush = this.reverseProxy.AutoFlush || this.RawReq.Header.Get("Accept") == "text/event-stream"

	// 准备
	var delayHeaders = this.writer.Prepare(resp, resp.ContentLength, resp.StatusCode, true)

	// 设置响应代码
	if !delayHeaders {
		this.writer.WriteHeader(resp.StatusCode)
	}

	// 是否有内容
	if resp.ContentLength == 0 && len(resp.TransferEncoding) == 0 {
		// 即使内容为0，也需要读取一次，以便于触发相关事件
		var buf = utils.BytePool4k.Get()
		_, _ = io.CopyBuffer(this.writer, resp.Body, buf)
		utils.BytePool4k.Put(buf)
		_ = resp.Body.Close()

		this.writer.SetOk()
		return
	}

	// 输出到客户端
	var pool = this.bytePool(resp.ContentLength)
	var buf = pool.Get()
	if shouldAutoFlush {
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				_, err = this.writer.Write(buf[:n])
				this.writer.Flush()
				if err != nil {
					break
				}
			}
			if readErr != nil {
				err = readErr
				break
			}
		}
	} else {
		_, err = io.CopyBuffer(this.writer, resp.Body, buf)
	}
	pool.Put(buf)

	var closeErr = resp.Body.Close()
	if closeErr != nil {
		if !this.canIgnore(closeErr) {
			remotelogs.WarnServer("HTTP_REQUEST_REVERSE_PROXY", this.URL()+": Closing error: "+closeErr.Error())
		}
	}

	if err != nil && err != io.EOF {
		if !this.canIgnore(err) {
			remotelogs.WarnServer("HTTP_REQUEST_REVERSE_PROXY", this.URL()+": Writing error: "+err.Error())
			this.addError(err)
		}
	}

	// 是否成功结束
	if (err == nil || err == io.EOF) && (closeErr == nil || closeErr == io.EOF) {
		this.writer.SetOk()
	}

	return
}
