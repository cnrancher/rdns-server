package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/etcd/client"
	"github.com/rancher/rdns-server/model"
	"github.com/rancher/rdns-server/util"
	"github.com/sirupsen/logrus"
)

const (
	BackendName  = "etcd"
	ValueHostKey = "host"
	ValueTextKey = "text"
	DefaultTTL   = "240h"

	maxSlugHashTimes = 100
	tokenOriginPath  = "/token_origin"

	slugLength        = 6
	tokenOriginLength = 32
)

type BackendOperator struct {
	kapi       client.KeysAPI
	prePath    string
	duration   time.Duration
	rootDomain string
}

func NewEtcdBackend(endpoints []string, prePath string, rootDomain string) (*BackendOperator, error) {
	logrus.Debugf("Etcd init...")
	cfg := client.Config{
		Endpoints: endpoints,
		Transport: client.DefaultTransport,
		// set timeout per request to fail fast when the target endpoint is unavailable
		HeaderTimeoutPerRequest: time.Second,
	}
	c, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	kapi := client.NewKeysAPI(c)

	duration, err := time.ParseDuration(DefaultTTL)
	if err != nil {
		return nil, err
	}

	return &BackendOperator{kapi, prePath, duration, rootDomain}, nil
}

func (e *BackendOperator) path(domainName string) string {
	return e.prePath + convertToPath(domainName)
}

func (e *BackendOperator) tokenOriginPath(domainName string) string {
	return tokenOriginPath + "/" + formatKey(domainName)
}

func (e *BackendOperator) lookupHosts(path string) (hosts []string, err error) {
	opts := &client.GetOptions{Recursive: true}
	resp, err := e.kapi.Get(context.Background(), path, opts)
	if err != nil {
		return hosts, err
	}
	for _, n := range resp.Node.Nodes {
		v, err := convertToMap(n.Value)
		if err != nil {
			return hosts, err
		}
		hosts = append(hosts, v[ValueHostKey])
	}

	return hosts, nil
}

func (e *BackendOperator) refreshExpiration(path string, dopts *model.DomainOptions) (d model.Domain, err error) {
	err = e.setTokenOrigin(dopts, true)
	if err != nil {
		return d, err
	}

	logrus.Debugf("Etcd: refresh dir TTL: %s", path)
	opts := &client.SetOptions{TTL: e.duration, Dir: true, PrevExist: client.PrevExist}
	resp, err := e.kapi.Set(context.Background(), path, "", opts)
	if err != nil {
		return d, err
	}

	curHosts, err := e.lookupHosts(path)
	if err != nil {
		return d, err
	}

	d.Fqdn = dopts.Fqdn
	d.Hosts = curHosts
	d.Expiration = resp.Node.Expiration

	// acme text record should be refresh expiration
	getOpts := &client.GetOptions{Sort: true, Recursive: true}
	getResp, err := e.kapi.Get(context.Background(), e.prePath+"/_txt/_acme-challenge", getOpts)
	if err != nil {
		return d, err
	}

	subKeys := nodesToStringSlice(getResp.Node.Nodes)
	for _, key := range subKeys {
		splits := strings.Split(dopts.Fqdn, ".")
		source := strings.Join(splits, "/")
		if strings.Contains(key, source) {
			// get value and refresh expiration
			getResp, err = e.kapi.Get(context.Background(), key, &client.GetOptions{})
			if err != nil {
				return d, err
			}
			acmeOpts := &client.SetOptions{TTL: e.duration, PrevExist: client.PrevExist}
			_, err := e.kapi.Set(context.Background(), key, getResp.Node.Value, acmeOpts)
			if err != nil {
				return d, err
			}
		}
	}

	return d, err
}

func (e *BackendOperator) setTokenOrigin(dopts *model.DomainOptions, exist bool) error {
	var tokenOrigin string
	opts := &client.SetOptions{TTL: e.duration}
	if exist {
		opts.PrevExist = client.PrevExist
	}
	tokenOriginPath := e.tokenOriginPath(dopts.Fqdn)
	resp, err := e.kapi.Get(context.Background(), tokenOriginPath, nil)
	if resp != nil {
		tokenOrigin = resp.Node.Value
		logrus.Debugf("setTokenOrigin: Got an exist token origin: %s", tokenOrigin)
	} else {
		tokenOrigin = generateTokenOrigin()
		logrus.Debugf("setTokenOrigin: Generrated a new token origin: %s", tokenOrigin)
	}

	logrus.Debugf("Etcd: set a path for token origin: %s, %s", tokenOriginPath, tokenOrigin)
	_, err = e.kapi.Set(context.Background(), tokenOriginPath, tokenOrigin, opts)
	return err
}

func (e *BackendOperator) set(path string, dopts *model.DomainOptions, exist bool) (d model.Domain, err error) {
	err = e.setTokenOrigin(dopts, exist)
	if err != nil {
		return d, err
	}

	// set domain record
	logrus.Debugf("Etcd: set a dir for record: %s", path)
	opts := &client.SetOptions{TTL: e.duration, Dir: true}
	if exist {
		opts.PrevExist = client.PrevExist
	}
	resp, err := e.kapi.Set(context.Background(), path, "", opts)
	if err != nil {
		return d, err
	}

	// get current hosts
	curHosts, err := e.lookupHosts(path)
	if err != nil {
		return d, err
	}

	// set key value
	newHostsMap := sliceToMap(dopts.Hosts)
	logrus.Debugf("Got new hosts map: %v", newHostsMap)
	oldHostsMap := sliceToMap(curHosts)
	logrus.Debugf("Got old hosts map: %v", oldHostsMap)
	for oldh := range oldHostsMap {
		if _, ok := newHostsMap[oldh]; !ok {
			key := fmt.Sprintf("%s/%s", path, formatKey(oldh))
			logrus.Debugf("Etcd: delete a key/value: %s:%s", key, formatValue(oldh))
			_, err := e.kapi.Delete(context.Background(), key, nil)
			if err != nil {
				return d, err
			}
		}
	}
	for newh := range newHostsMap {
		if _, ok := oldHostsMap[newh]; !ok {
			key := fmt.Sprintf("%s/%s", path, formatKey(newh))
			logrus.Debugf("Etcd: set a key/value: %s:%s", key, formatValue(newh))
			_, err := e.kapi.Set(context.Background(), key, formatValue(newh), nil)
			if err != nil {
				return d, err
			}
		}
	}

	d.Fqdn = dopts.Fqdn
	d.Hosts = dopts.Hosts
	d.Expiration = resp.Node.Expiration
	logrus.Debugf("Finished to set a domain entry: %s", d.String())

	return d, nil
}

func (e *BackendOperator) setText(path string, dopts *model.DomainOptions, exist bool) (d model.Domain, err error) {
	opts := &client.SetOptions{TTL: e.duration}
	if exist {
		opts.PrevExist = client.PrevExist
	}
	resp, err := e.kapi.Set(context.Background(), path, formatTextValue(dopts.Text), opts)
	if err != nil {
		return d, err
	}

	d.Fqdn = dopts.Fqdn
	d.Text = dopts.Text
	d.Expiration = resp.Node.Expiration
	logrus.Debugf("Finished to set a domain entry: %s", d.String())

	return d, nil
}

func (e *BackendOperator) Get(dopts *model.DomainOptions) (d model.Domain, err error) {
	logrus.Debugf("Get in etcd: Got the domain options entry: %s", dopts.String())
	path := e.path(dopts.Fqdn)
	//opts := &client.GetOptions{Recursive: true}
	resp, err := e.kapi.Get(context.Background(), path, nil)
	if err != nil {
		return d, err
	}

	d.Fqdn = dopts.Fqdn
	d.Expiration = resp.Node.Expiration
	for _, n := range resp.Node.Nodes {
		if n.Dir {
			continue
		}
		v, err := convertToMap(n.Value)
		if err != nil {
			return d, err
		}
		d.Hosts = append(d.Hosts, v[ValueHostKey])
	}

	return d, nil
}

func (e *BackendOperator) Create(dopts *model.DomainOptions) (d model.Domain, err error) {
	logrus.Debugf("Create in etcd: Got the domain options entry: %s", dopts.String())
	var path string
	for i := 0; i < maxSlugHashTimes; i++ {
		fqdn := fmt.Sprintf("%s.%s", generateSlug(), e.rootDomain)
		path = e.path(fqdn)

		// check if this path exists and use this path if not exist
		_, err := e.kapi.Get(context.Background(), path, nil)
		if err != nil && client.IsKeyNotFound(err) {
			dopts.Fqdn = fqdn
			break
		}
	}

	d, err = e.set(path, dopts, false)
	if err != nil {
		return d, err
	}

	return e.Get(dopts)
}

func (e *BackendOperator) Update(dopts *model.DomainOptions) (d model.Domain, err error) {
	exist := false
	logrus.Debugf("Update in etcd: Got the domain options entry: %s", dopts.String())
	path := e.path(dopts.Fqdn)

	resp, err := e.kapi.Get(context.Background(), path, &client.GetOptions{})
	if err == nil && resp != nil && resp.Node.Dir {
		logrus.Debugf("%s: is a directory", resp.Node.Key)
		exist = true
	}

	d, err = e.set(path, dopts, exist)
	return d, err
}

func (e *BackendOperator) Renew(dopts *model.DomainOptions) (d model.Domain, err error) {
	logrus.Debugf("Renew in etcd: Got the domain options entry: %s", dopts.String())
	path := e.path(dopts.Fqdn)

	d, err = e.refreshExpiration(path, dopts)
	return d, err
}

func (e *BackendOperator) Delete(dopts *model.DomainOptions) error {
	logrus.Debugf("Delete in etcd: Got the domain options entry: %s", dopts.String())
	path := e.path(dopts.Fqdn)

	opts := &client.DeleteOptions{Dir: true, Recursive: true}
	_, err := e.kapi.Delete(context.Background(), path, opts)
	return err
}

func (e *BackendOperator) CreateText(dopts *model.DomainOptions) (d model.Domain, err error) {
	logrus.Debugf("Create in etcd: Got the domain options entry: %s", dopts.String())

	fqdn := dopts.Fqdn
	var path string
	// acme text record: _acme-challenge.x1.lb.rancher.cloud
	if strings.Contains(fqdn, "_acme-challenge") {
		// need save to the path /rdns/_txt/_acme-challenge/x1/lb/rancher/cloud
		temp := fmt.Sprintf("%s.%s", "_txt", fqdn)
		tempSlice := strings.Split(temp, ".")
		path = fmt.Sprintf("%s/%s", e.prePath, strings.Join(tempSlice, "/"))
	} else {
		// normal text record: xxxx.lb.rancher.cloud
		path = e.path(fqdn)
	}

	exist := true
	// check if this path exists
	_, err = e.kapi.Get(context.Background(), path, nil)
	if err != nil && client.IsKeyNotFound(err) {
		exist = false
	}

	d, err = e.setText(path, dopts, exist)
	if err != nil {
		return d, err
	}

	return d, err
}

func (e *BackendOperator) GetText(dopts *model.DomainOptions) (d model.Domain, err error) {
	logrus.Debugf("Get in etcd: Got the domain options entry: %s", dopts.String())
	fqdn := dopts.Fqdn
	var path string
	// acme text record: _acme-challenge.x1.lb.rancher.cloud
	if strings.Contains(fqdn, "_acme-challenge") {
		// need save to the path /rdns/_txt/_acme-challenge/x1/lb/rancher/cloud
		temp := fmt.Sprintf("%s.%s", "_txt", fqdn)
		tempSlice := strings.Split(temp, ".")
		path = fmt.Sprintf("%s/%s", e.prePath, strings.Join(tempSlice, "/"))
	} else {
		// normal text record: xxxx.lb.rancher.cloud
		path = e.path(fqdn)
	}

	//opts := &client.GetOptions{Recursive: true}
	resp, err := e.kapi.Get(context.Background(), path, nil)
	if err != nil {
		return d, err
	}

	d.Fqdn = dopts.Fqdn
	d.Expiration = resp.Node.Expiration
	d.Text = resp.Node.Value

	return d, nil
}

func (e *BackendOperator) UpdateText(dopts *model.DomainOptions) (d model.Domain, err error) {
	exist := false
	logrus.Debugf("Update in etcd: Got the domain options entry: %s", dopts.String())
	fqdn := dopts.Fqdn
	var path string
	// acme text record: _acme-challenge.x1.lb.rancher.cloud
	if strings.Contains(fqdn, "_acme-challenge") {
		// need save to the path /rdns/_txt/_acme-challenge/x1/lb/rancher/cloud
		temp := fmt.Sprintf("%s.%s", "_txt", fqdn)
		tempSlice := strings.Split(temp, ".")
		path = fmt.Sprintf("%s/%s", e.prePath, strings.Join(tempSlice, "/"))
	} else {
		// normal text record: xxxx.lb.rancher.cloud
		path = e.path(fqdn)
	}

	resp, err := e.kapi.Get(context.Background(), path, &client.GetOptions{})
	if err == nil && resp != nil && resp.Node.Dir {
		logrus.Debugf("%s: is a directory", resp.Node.Key)
		exist = true
	}

	d, err = e.setText(path, dopts, exist)
	return d, err
}

func (e *BackendOperator) DeleteText(dopts *model.DomainOptions) error {
	logrus.Debugf("Delete in etcd: Got the domain options entry: %s", dopts.String())
	fqdn := dopts.Fqdn
	var path string
	// acme text record: _acme-challenge.x1.lb.rancher.cloud
	if strings.Contains(fqdn, "_acme-challenge") {
		// need save to the path /rdns/_txt/_acme-challenge/x1/lb/rancher/cloud
		temp := fmt.Sprintf("%s.%s", "_txt", fqdn)
		tempSlice := strings.Split(temp, ".")
		path = fmt.Sprintf("%s/%s", e.prePath, strings.Join(tempSlice, "/"))
	} else {
		// normal text record: xxxx.lb.rancher.cloud
		path = e.path(fqdn)
	}

	opts := &client.DeleteOptions{Dir: true, Recursive: true}
	_, err := e.kapi.Delete(context.Background(), path, opts)
	return err
}

func (e *BackendOperator) GetTokenOrigin(fqdn string) (string, error) {
	logrus.Debugf("Get key for token in etcd: fqdn: %s", fqdn)
	tokenOriginPath := e.tokenOriginPath(fqdn)
	resp, err := e.kapi.Get(context.Background(), tokenOriginPath, nil)
	if err != nil {
		return "", err
	}

	origin := resp.Node.Value
	logrus.Debugf("The token origin is %s", origin)

	return origin, nil
}

// convertToPath
// zhibo.test.rancher.local => /local/rancher/test/zhibo
func convertToPath(domain string) string {
	ss := strings.Split(domain, ".")

	last := len(ss) - 1
	for i := 0; i < len(ss)/2; i++ {
		ss[i], ss[last-i] = ss[last-i], ss[i]
	}

	return "/" + strings.Join(ss, "/")
}

// convertToMap
// source data: {"host":"1.1.1.1"}
func convertToMap(value string) (map[string]string, error) {
	var v map[string]string
	err := json.Unmarshal([]byte(value), &v)
	return v, err
}

// formatValue
// 1.1.1.1 => {"host": "1.1.1.1"}
func formatValue(value string) string {
	return fmt.Sprintf("{\"%s\":\"%s\"}", ValueHostKey, value)
}

func formatTextValue(value string) string {
	return fmt.Sprintf("{\"%s\":\"%s\"}", ValueTextKey, value)
}

// formatKey
// 1.1.1.1 => 1_1_1_1
// abcdef.lb.rancher.cloud => abcdef_lb_rancher_cloud
func formatKey(key string) string {
	return strings.Replace(key, ".", "_", -1)
}

func sliceToMap(ss []string) map[string]bool {
	m := make(map[string]bool)
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// generateSlug will generate a random slug to be used as shorten link.
func generateSlug() string {
	return util.RandStringWithSmall(slugLength)
}

func generateTokenOrigin() string {
	return util.RandStringWithAll(tokenOriginLength)
}

func nodesToStringSlice(nodes client.Nodes) []string {
	var keys []string

	for _, node := range nodes {
		keys = append(keys, node.Key)

		for _, key := range nodesToStringSlice(node.Nodes) {
			keys = append(keys, key)
		}
	}

	return keys
}
