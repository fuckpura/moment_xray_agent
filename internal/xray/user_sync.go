package xray

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/xtls/xray-core/common/protocol"
	xraycore "github.com/xtls/xray-core/core"
	xrayinbound "github.com/xtls/xray-core/features/inbound"
	confserial "github.com/xtls/xray-core/infra/conf/serial"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/hysteria"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	shadowsocks2022 "github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"github.com/xtls/xray-core/proxy/trojan"
	vlessin "github.com/xtls/xray-core/proxy/vless/inbound"
	vmessin "github.com/xtls/xray-core/proxy/vmess/inbound"
	"google.golang.org/protobuf/proto"
)

type dynamicInboundUsers struct {
	supported bool
	byTag     map[string]map[string]dynamicUser
}

type dynamicUser struct {
	email       string
	fingerprint string
	user        *protocol.User
}

type userChangeSet struct {
	remove []userRemoval
	add    []userAddition
}

type userRemoval struct {
	tag   string
	email string
}

type userAddition struct {
	tag  string
	user *protocol.User
}

func extractDynamicInboundUsers(configData []byte) (dynamicInboundUsers, error) {
	decoded, err := confserial.DecodeJSONConfig(bytes.NewReader(configData))
	if err != nil {
		return dynamicInboundUsers{}, fmt.Errorf("decode runtime config for user sync: %w", err)
	}
	result := dynamicInboundUsers{
		byTag: map[string]map[string]dynamicUser{},
	}
	for _, inboundConfig := range decoded.InboundConfigs {
		built, err := inboundConfig.Build()
		if err != nil {
			return dynamicInboundUsers{}, fmt.Errorf("build inbound %q for user sync: %w", inboundConfig.Tag, err)
		}
		users, managed := extractCoreInboundUsers(built)
		if !managed {
			continue
		}
		if inboundConfig.Tag == "" {
			return dynamicInboundUsers{}, errors.New("dynamic user inbound must have a tag for hot sync")
		}
		result.supported = true
		tagUsers := make(map[string]dynamicUser, len(users))
		for _, user := range users {
			if user == nil || user.Email == "" {
				return dynamicInboundUsers{}, fmt.Errorf("dynamic user inbound %q has an empty user email", inboundConfig.Tag)
			}
			fingerprint, err := dynamicUserFingerprint(user)
			if err != nil {
				return dynamicInboundUsers{}, fmt.Errorf("fingerprint user %q on inbound %q: %w", user.Email, inboundConfig.Tag, err)
			}
			tagUsers[user.Email] = dynamicUser{
				email:       user.Email,
				fingerprint: fingerprint,
				user:        proto.Clone(user).(*protocol.User),
			}
		}
		result.byTag[inboundConfig.Tag] = tagUsers
	}
	return result, nil
}

func extractCoreInboundUsers(inboundConfig *xraycore.InboundHandlerConfig) ([]*protocol.User, bool) {
	if inboundConfig == nil || inboundConfig.ProxySettings == nil {
		return nil, false
	}
	instance, err := inboundConfig.ProxySettings.GetInstance()
	if err != nil || instance == nil {
		return nil, false
	}
	switch typed := instance.(type) {
	case *vmessin.Config:
		return typed.User, true
	case *vlessin.Config:
		return typed.Clients, true
	case *trojan.ServerConfig:
		return typed.Users, true
	case *shadowsocks.ServerConfig:
		return typed.Users, true
	case *shadowsocks2022.MultiUserServerConfig:
		return typed.Users, true
	case *hysteria.ServerConfig:
		return typed.Users, true
	default:
		return nil, false
	}
}

func dynamicUserFingerprint(user *protocol.User) (string, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(user)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func diffDynamicInboundUsers(current dynamicInboundUsers, target dynamicInboundUsers) userChangeSet {
	changes := userChangeSet{}
	for _, tag := range sortedTags(current.byTag) {
		currentUsers := current.byTag[tag]
		targetUsers := target.byTag[tag]
		for _, email := range sortedEmails(currentUsers) {
			currentUser := currentUsers[email]
			nextUser, exists := targetUsers[email]
			if !exists || currentUser.fingerprint != nextUser.fingerprint {
				changes.remove = append(changes.remove, userRemoval{tag: tag, email: email})
			}
		}
	}
	for _, tag := range sortedTags(target.byTag) {
		currentUsers := current.byTag[tag]
		targetUsers := target.byTag[tag]
		for _, email := range sortedEmails(targetUsers) {
			nextUser := targetUsers[email]
			currentUser, exists := currentUsers[email]
			if !exists || currentUser.fingerprint != nextUser.fingerprint {
				changes.add = append(changes.add, userAddition{tag: tag, user: nextUser.user})
			}
		}
	}
	return changes
}

func (c userChangeSet) empty() bool {
	return len(c.remove) == 0 && len(c.add) == 0
}

func (r *Runtime) applyUserChangesLocked(ctx context.Context, changes userChangeSet) error {
	manager, err := r.inboundManagerLocked()
	if err != nil {
		return err
	}
	for _, removal := range changes.remove {
		userManager, err := runtimeUserManager(ctx, manager, removal.tag)
		if err != nil {
			return err
		}
		if err := userManager.RemoveUser(ctx, removal.email); err != nil {
			return fmt.Errorf("remove user %q from inbound %q: %w", removal.email, removal.tag, err)
		}
	}
	for _, addition := range changes.add {
		userManager, err := runtimeUserManager(ctx, manager, addition.tag)
		if err != nil {
			return err
		}
		memoryUser, err := addition.user.ToMemoryUser()
		if err != nil {
			return fmt.Errorf("parse user %q for inbound %q: %w", addition.user.Email, addition.tag, err)
		}
		if err := userManager.AddUser(ctx, memoryUser); err != nil {
			return fmt.Errorf("add user %q to inbound %q: %w", addition.user.Email, addition.tag, err)
		}
	}
	return nil
}

func (r *Runtime) inboundManagerLocked() (xrayinbound.Manager, error) {
	if r.instance == nil {
		return nil, errors.New("xray runtime is not running")
	}
	var manager xrayinbound.Manager
	if err := r.instance.RequireFeatures(func(inboundManager xrayinbound.Manager) {
		manager = inboundManager
	}, false); err != nil {
		return nil, err
	}
	if manager == nil {
		return nil, errors.New("xray inbound manager is not available")
	}
	return manager, nil
}

func runtimeUserManager(ctx context.Context, manager xrayinbound.Manager, tag string) (proxy.UserManager, error) {
	handler, err := manager.GetHandler(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("get inbound %q: %w", tag, err)
	}
	rawInbound, ok := handler.(proxy.GetInbound)
	if !ok {
		return nil, fmt.Errorf("inbound %q does not expose a proxy", tag)
	}
	userManager, ok := rawInbound.GetInbound().(proxy.UserManager)
	if !ok {
		return nil, fmt.Errorf("inbound %q does not support dynamic users", tag)
	}
	return userManager, nil
}

func sortedTags(users map[string]map[string]dynamicUser) []string {
	tags := make([]string, 0, len(users))
	for tag := range users {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

func sortedEmails(users map[string]dynamicUser) []string {
	emails := make([]string, 0, len(users))
	for email := range users {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	return emails
}

func (u dynamicInboundUsers) clone() dynamicInboundUsers {
	clone := dynamicInboundUsers{
		supported: u.supported,
		byTag:     make(map[string]map[string]dynamicUser, len(u.byTag)),
	}
	for tag, users := range u.byTag {
		tagUsers := make(map[string]dynamicUser, len(users))
		for email, user := range users {
			copied := user
			if user.user != nil {
				copied.user = proto.Clone(user.user).(*protocol.User)
			}
			tagUsers[email] = copied
		}
		clone.byTag[tag] = tagUsers
	}
	return clone
}
