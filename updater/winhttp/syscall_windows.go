//go:build windows

package winhttp

import (
	"golang.org/x/sys/windows"
)

type _HINTERNET windows.Handle

type Error uint32

const (
	_WINHTTP_ACCESS_TYPE_DEFAULT_PROXY   = 0
	_WINHTTP_ACCESS_TYPE_NO_PROXY        = 1
	_WINHTTP_ACCESS_TYPE_NAMED_PROXY     = 3
	_WINHTTP_ACCESS_TYPE_AUTOMATIC_PROXY = 4

	_WINHTTP_FLAG_ASYNC = 0x10000000

	_WINHTTP_INVALID_STATUS_CALLBACK = ^uintptr(0)

	_WINHTTP_FLAG_SECURE               = 0x00800000
	_WINHTTP_FLAG_BYPASS_PROXY_CACHE   = 0x00000100
	_WINHTTP_FLAG_REFRESH              = _WINHTTP_FLAG_BYPASS_PROXY_CACHE

	_WINHTTP_QUERY_CONTENT_LENGTH = 5
	_WINHTTP_QUERY_STATUS_CODE    = 19
	_WINHTTP_QUERY_FLAG_NUMBER    = 0x20000000

	_WINHTTP_OPTION_ENABLE_HTTP_PROTOCOL = 133
	_WINHTTP_OPTION_SECURE_PROTOCOLS     = 84

	_WINHTTP_PROTOCOL_FLAG_HTTP2 = 0x1

	_WINHTTP_FLAG_SECURE_PROTOCOL_TLS1_2 = 0x00000800

	_WINHTTP_ERROR_BASE = 12000
	_WINHTTP_ERROR_LAST = _WINHTTP_ERROR_BASE + 190
)

type _URL_COMPONENTS struct {
	structSize      uint32
	scheme          *uint16
	schemeLength    uint32
	schemeType      uint32
	hostName        *uint16
	hostNameLength  uint32
	port            uint16
	username        *uint16
	usernameLength  uint32
	password        *uint16
	passwordLength  uint32
	urlPath         *uint16
	urlPathLength   uint32
	extraInfo       *uint16
	extraInfoLength uint32
}

//sys	winHttpOpen(userAgent *uint16, accessType uint32, proxy *uint16, proxyBypass *uint16, flags uint32) (sessionHandle _HINTERNET, err error) = winhttp.WinHttpOpen
//sys	winHttpCloseHandle(handle _HINTERNET) (err error) = winhttp.WinHttpCloseHandle
//sys	winHttpConnect(sessionHandle _HINTERNET, serverName *uint16, serverPort uint16, reserved uint32) (handle _HINTERNET, err error) = winhttp.WinHttpConnect
//sys	winHttpOpenRequest(connectHandle _HINTERNET, verb *uint16, objectName *uint16, version *uint16, referrer *uint16, acceptTypes **uint16, flags uint32) (requestHandle _HINTERNET, err error) = winhttp.WinHttpOpenRequest
//sys	winHttpSendRequest(requestHandle _HINTERNET, headers *uint16, headersLength uint32, optional *byte, optionalLength uint32, totalLength uint32, context uintptr) (err error) = winhttp.WinHttpSendRequest
//sys	winHttpReceiveResponse(requestHandle _HINTERNET, reserved uintptr) (err error) = winhttp.WinHttpReceiveResponse
//sys	winHttpQueryHeaders(requestHandle _HINTERNET, infoLevel uint32, name *uint16, buffer unsafe.Pointer, bufferLen *uint32, index *uint32) (err error) = winhttp.WinHttpQueryHeaders
//sys	winHttpReadData(requestHandle _HINTERNET, buffer *byte, bufferSize uint32, bytesRead *uint32) (err error) = winhttp.WinHttpReadData
//sys	winHttpSetOption(sessionOrRequestHandle _HINTERNET, option uint32, buffer unsafe.Pointer, bufferLen uint32) (err error) = winhttp.WinHttpSetOption

