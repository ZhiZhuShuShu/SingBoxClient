package outbound

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/convert"
	"github.com/sagernet/sing-box/common/json"
	"github.com/sagernet/sing-box/common/timer"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service/filemanager"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	_ adapter.Outbound      = (*Provider)(nil)
	_ adapter.OutboundGroup = (*Provider)(nil)
)

var (
	fileMode os.FileMode = 0o666
	dirMode  os.FileMode = 0o755
)

type Provider struct {
	myOutboundAdapter
	ctx             context.Context
	providerType    string
	url             option.Listable[string]
	path            option.Listable[string]
	defaultTag      string
	outbounds       map[string]adapter.Outbound
	selected        adapter.Outbound
	tags            []string
	interval        string
	policy          string // urltest, loadbalance, select
	urlTest         *option.UrlTest
	group           *URLTestGroup
	loadBalance     bool
	includeKeyWords option.Listable[string]
	excludeKeyWords option.Listable[string]
}

func NewProvider(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.ProviderOutboundOptions) (*Provider, error) {
	outbound := &Provider{
		ctx: ctx,
		myOutboundAdapter: myOutboundAdapter{
			protocol: C.TypeProvider,
			router:   router,
			logger:   logger,
			tag:      tag,
		},
		defaultTag:      options.Default,
		outbounds:       make(map[string]adapter.Outbound),
		url:             options.Url,
		path:            options.Path,
		providerType:    options.ProviderType,
		interval:        options.Interval,
		urlTest:         options.UrlTest,
		includeKeyWords: options.IncludeKeyWords,
		excludeKeyWords: options.ExcludeKeyWords,
	}

	if outbound.interval == "" {
		outbound.interval = "1h"
	}

	if outbound.policy == "" {
		outbound.policy = "urlTest"
	}

	if outbound.urlTest == nil {
		outbound.urlTest = &option.UrlTest{
			Url:      "http://www.gstatic.com/generate_204",
			Interval: "1m",
		}
	}

	if outbound.providerType == "url" && len(outbound.url) == 0 {
		return nil, E.New("missing provider url")
	}

	if outbound.providerType == "file" && len(outbound.path) == 0 {
		return nil, E.New("missing provider path")
	}

	if outbound.providerType == "url" && len(outbound.path) == 0 {
		for _, v := range outbound.url {
			providerPath := filepath.Join("provider", tag+"_"+md5V(v)+".txt")
			outbound.path = append(outbound.path, filemanager.BasePath(ctx, providerPath))
		}
	}

	return outbound, nil
}

func (s *Provider) Network() []string {
	if s.group != nil {
		return s.group.Select(N.NetworkTCP).Network()
	}
	return s.selected.Network()
}

func (s *Provider) getProviderContent(url, path string) ([]byte, error) {
	bt, err := loadPath(s.ctx, path)
	if err == nil {
		return bt, nil
	}
	return loadUrl(s.ctx, url, path)
}

func (s *Provider) Start() error {
	if s.defaultTag != "" {
		detour, loaded := s.router.Outbound(s.defaultTag)
		if !loaded {
			return E.New("default outbound not found: ", s.defaultTag)
		}
		s.selected = detour
	}

	switch s.providerType {
	case "file":
		for _, v := range s.path {
			content, err := loadPath(s.ctx, v)
			if err != nil {
				return err
			}
			s.logger.Debug("loadPath ", v)
			err = s.parseProvider(s.ctx, content)
			if err != nil {
				return E.Extend(err, "parseProvider fail")
			}
		}
	case "url":
		for i, v := range s.url {
			content, err := s.getProviderContent(v, s.path[i])
			if err != nil {
				return err
			}
			s.logger.Debug("loadUrl ", v)

			err = s.parseProvider(s.ctx, content)
			if err != nil {
				return E.Extend(err, "parseProvider fail")
			}
		}

		if len(s.url) > 0 {
			interval, err := time.ParseDuration(s.interval)
			if err != nil {
				return err
			}

			timer.Timer(interval, func() {
				for i, v := range s.url {
					content, err := loadUrl(s.ctx, v, s.path[i])
					if err != nil {
						s.logger.Error("loadUrl ", v, " fail")
						return
					}
					s.logger.Debug("loadUrl ", v)

					err = s.parseProvider(s.ctx, content)
					if err != nil {
						s.logger.Error("parseProvider ", v, " fail")
					}
				}
			}, false)
		}
	}

	s.logger.Debug("provider init ", s.myOutboundAdapter.tag, " ", len(s.outbounds))

	return nil
}

func (s *Provider) Now() string {
	return s.getSelected(N.NetworkTCP).Tag()
}

func (s *Provider) All() []string {
	return s.tags
}

func (s *Provider) AllOutbound() map[string]adapter.Outbound {
	return s.outbounds
}

func (s *Provider) SelectOutbound(tag string) bool {
	detour, loaded := s.outbounds[tag]
	if !loaded {
		return false
	}
	s.selected = detour
	return true
}

func (s *Provider) updateSelected(outboundMap map[string]adapter.Outbound) error {
	switch s.policy {
	case "urlTest":
		interval, err := time.ParseDuration(s.urlTest.Interval)
		if err != nil {
			return err
		}

		var outbounds []adapter.Outbound
		for _, v := range outboundMap {
			outbounds = append(outbounds, v)
		}

		s.logger.Debug("NewURLTestGroup ", len(outbounds))

		s.group = NewURLTestGroup(context.Background(), s.router, s.logger, outbounds, s.urlTest.Url, interval, 100)
		s.group.Start()
		s.logger.Debug("start NewURLTestGroup")

	case "loadBalance":
		if s.loadBalance == false {
			timer.Timer(time.Minute*10, func() {
				index := rand.Intn(len(s.tags))
				s.selected = outboundMap[s.tags[index]]
				s.logger.Debug("selected by loadBalance ", s.selected.Tag())
			}, true)
			s.loadBalance = true
		}

	default:
		s.logger.Error("not support provider policy ", s.policy)
	}
	return nil
}

func (s *Provider) getSelected(network string) adapter.Outbound {
	if s.group != nil {
		return s.group.Select(network)
	}
	return s.selected
}

func (s *Provider) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return s.getSelected(network).DialContext(ctx, network, destination)
}

func (s *Provider) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return s.getSelected(N.NetworkUDP).ListenPacket(ctx, destination)
}

func (s *Provider) NewConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	return s.getSelected(N.NetworkTCP).NewConnection(ctx, conn, metadata)
}

func (s *Provider) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	return s.getSelected(N.NetworkUDP).NewPacketConnection(ctx, conn, metadata)
}

func (s *Provider) parseContent(content []byte) (obs []option.Outbound, err error) {
	if err := json.Unmarshal(content, &obs); err == nil {
		return obs, nil
	} else {
		s.logger.Debug("sing-box conf json.Unmarshal ", err.Error())
	}

	if detail, err := convert.ConvertsV2Ray(content); err == nil {
		if jsonDetail, err := json.Marshal(detail); err == nil {
			if err = json.Unmarshal(jsonDetail, &obs); err == nil {
				return obs, nil
			} else {
				s.logger.Debug("convert.ConvertsV2Ray json.Unmarshal", err.Error())
			}
		} else {
			s.logger.Debug("convert.ConvertsV2Ray", err.Error())
		}
	}

	if obs, err := convert.ConvertsClash(content); err == nil {
		return obs, nil
	}

	return nil, E.Extend(err, "can not parse sub link")
}

func (s *Provider) parseProvider(ctx context.Context, content []byte) error {
	outbounds, err := s.parseContent(content)
	if err != nil {
		return err
	}

	if len(outbounds) == 0 {
		return E.New("provider outbounds is empty")
	}

	for _, v := range outbounds {
		detour, err := New(ctx, s.router, s.logger, v.Tag, v)
		if err != nil {
			return E.Extend(err, "New.outbound")
		}

		if len(s.includeKeyWords) > 0 && miss(detour.Tag(), s.includeKeyWords) {
			s.logger.Debug("parseProvider filter by includeKeyWords ", detour.Tag())
			continue
		}

		if len(s.excludeKeyWords) > 0 && match(detour.Tag(), s.excludeKeyWords) {
			s.logger.Debug("parseProvider filter by excludeKeyWords ", detour.Tag())
			continue
		}

		s.outbounds[detour.Tag()] = detour
		s.tags = append(s.tags, detour.Tag())
		s.tags = arrUnique(s.tags)

	}

	if len(s.outbounds) == 0 {
		s.logger.Error("parseContent outbounds ", len(outbounds), " -> ", len(s.outbounds), fmt.Sprintf(" %v %v", s.includeKeyWords, s.excludeKeyWords))
	}

	err = s.updateSelected(s.outbounds)
	if err != nil {
		s.logger.Error("updateSelected err ", err)
	}

	//if s.selected == nil {
	//	s.selected = s.outbounds[s.tags[0]]
	//	s.logger.Debug("first init selected ", s.selected.Tag())
	//}
	return nil
}

func match(str string, arr []string) bool {
	for _, v := range arr {
		if strings.Contains(str, v) {
			return true
		}
	}
	return false
}

func miss(str string, arr []string) bool {
	for _, v := range arr {
		if strings.Contains(str, v) {
			return false
		}
	}
	return true
}

func getHttpContent(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func loadUrl(ctx context.Context, url, path string) ([]byte, error) {
	content, err := getHttpContent(url)
	if err != nil {
		return nil, err
	}
	err = safeWrite(ctx, path, content)
	if err != nil {
		return nil, err
	}
	return content, nil
}

func loadPath(ctx context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

func safeWrite(ctx context.Context, path string, buf []byte) error {
	if parentDir := filepath.Dir(path); parentDir != "" {
		filemanager.MkdirAll(ctx, parentDir, 0o755)
	}

	saveFile, err := filemanager.Create(ctx, path)
	if err != nil {
		return E.Cause(err, "safeWrite file: ", path)
	}
	defer saveFile.Close()

	return filemanager.WriteFile(ctx, saveFile.Name(), buf, fileMode)
}

func md5V(str string) string {
	h := md5.New()
	h.Write([]byte(str))
	return hex.EncodeToString(h.Sum(nil))
}

func arrUnique(arr []string) (unique []string) {
	tmp := make(map[string]struct{})

	for _, v := range arr {
		if _, ok := tmp[v]; !ok {
			tmp[v] = struct{}{}
		}
	}

	for k, _ := range tmp {
		unique = append(unique, k)
	}

	return unique
}