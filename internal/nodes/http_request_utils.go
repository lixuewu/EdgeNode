package nodes

import (
	"crypto/rand"
	"fmt"
	teaconst "github.com/TeaOSLab/EdgeNode/internal/const"
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"github.com/TeaOSLab/EdgeNode/internal/utils/ranges"
	"github.com/iwind/TeaGo/types"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
)

// 其中的每个括号里的内容都在被引用，不能轻易修改
var contentRangeRegexp = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+|\*)`)

// 分解Range
func httpRequestParseRangeHeader(rangeValue string) (result []rangeutils.Range, ok bool) {
	// 参考RFC：https://tools.ietf.org/html/rfc7233
	index := strings.Index(rangeValue, "=")
	if index == -1 {
		return
	}
	unit := rangeValue[:index]
	if unit != "bytes" {
		return
	}

	var rangeSetString = rangeValue[index+1:]
	if len(rangeSetString) == 0 {
		ok = true
		return
	}

	var pieces = strings.Split(rangeSetString, ", ")
	for _, piece := range pieces {
		index = strings.Index(piece, "-")
		if index == -1 {
			return
		}
		first := piece[:index]
		firstInt := int64(-1)

		var err error
		last := piece[index+1:]
		var lastInt = int64(-1)

		if len(first) > 0 {
			firstInt, err = strconv.ParseInt(first, 10, 64)
			if err != nil {
				return
			}

			if len(last) > 0 {
				lastInt, err = strconv.ParseInt(last, 10, 64)
				if err != nil {
					return
				}
				if lastInt < firstInt {
					return
				}
			}
		} else {
			if len(last) == 0 {
				return
			}

			lastInt, err = strconv.ParseInt(last, 10, 64)
			if err != nil {
				return
			}
			lastInt = -lastInt
		}

		result = append(result, [2]int64{firstInt, lastInt})
	}

	ok = true
	return
}

// 读取内容Range
func httpRequestReadRange(reader io.Reader, buf []byte, start int64, end int64, callback func(buf []byte, n int) error) (ok bool, err error) {
	if start < 0 || end < 0 {
		return
	}
	seeker, ok := reader.(io.Seeker)
	if !ok {
		return
	}
	_, err = seeker.Seek(start, io.SeekStart)
	if err != nil {
		return false, nil
	}

	offset := start
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			offset += int64(n)
			if end < offset {
				err = callback(buf, n-int(offset-end-1))
				if err != nil {
					return false, err
				}
				return true, nil
			} else {
				err = callback(buf, n)
				if err != nil {
					return false, err
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				return true, nil
			}
			return false, err
		}
	}
}

// 分解Content-Range
func httpRequestParseContentRangeHeader(contentRange string) (start int64, total int64) {
	var matches = contentRangeRegexp.FindStringSubmatch(contentRange)
	if len(matches) < 4 {
		return -1, -1
	}

	start = types.Int64(matches[1])
	var sizeString = matches[3]
	if sizeString != "*" {
		total = types.Int64(sizeString)
	}
	return
}

// 生成boundary
// 仿照Golang自带的函数（multipart包）
func httpRequestGenBoundary() string {
	var buf [8]byte
	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", buf[:])
}

// 从content-type中读取boundary
func httpRequestParseBoundary(contentType string) string {
	var delim = "boundary="
	var boundaryIndex = strings.Index(contentType, delim)
	if boundaryIndex < 0 {
		return ""
	}
	var boundary = contentType[boundaryIndex+len(delim):]
	semicolonIndex := strings.Index(boundary, ";")
	if semicolonIndex >= 0 {
		return boundary[:semicolonIndex]
	}
	return boundary
}

// 判断状态是否为跳转
func httpStatusIsRedirect(statusCode int) bool {
	return statusCode == http.StatusPermanentRedirect ||
		statusCode == http.StatusTemporaryRedirect ||
		statusCode == http.StatusMovedPermanently ||
		statusCode == http.StatusSeeOther ||
		statusCode == http.StatusFound
}

// 生成请求ID
var httpRequestTimestamp int64
var httpRequestId int32 = 1_000_000

func httpRequestNextId() string {
	unixTime, unixTimeString := utils.UnixTimeMilliString()
	if unixTime > httpRequestTimestamp {
		atomic.StoreInt32(&httpRequestId, 1_000_000)
		httpRequestTimestamp = unixTime
	}

	// timestamp + nodeId + requestId
	return unixTimeString + teaconst.NodeIdString + strconv.Itoa(int(atomic.AddInt32(&httpRequestId, 1)))
}

// 检查是否可以接受某个编码
func httpAcceptEncoding(acceptEncodings string, encoding string) bool {
	if len(acceptEncodings) == 0 {
		return false
	}
	var pieces = strings.Split(acceptEncodings, ",")
	for _, piece := range pieces {
		var qualityIndex = strings.Index(piece, ";")
		if qualityIndex >= 0 {
			piece = piece[:qualityIndex]
		}

		if strings.TrimSpace(piece) == encoding {
			return true
		}
	}
	return false
}
