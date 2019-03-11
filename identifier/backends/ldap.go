/*
 * Copyright 2017 Kopano and its licensors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License, version 3,
 * as published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package backends

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"stash.kopano.io/kc/konnect"
	"stash.kopano.io/kc/konnect/config"
	ldapDefinitions "stash.kopano.io/kc/konnect/identifier/backends/ldap"
	"stash.kopano.io/kc/konnect/identifier/meta/scopes"
	"stash.kopano.io/kc/konnect/oidc"

	"github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	"gopkg.in/ldap.v2"
)

const ldapIdentifierBackendName = "identifier-ldap"

var ldapSupportedScopes = []string{
	oidc.ScopeProfile,
	oidc.ScopeEmail,
	konnect.ScopeUniqueUserID,
	konnect.ScopeRawSubject,
}

// LDAPIdentifierBackend is a backend for the Identifier which connects LDAP.
type LDAPIdentifierBackend struct {
	addr         string
	isTLS        bool
	bindDN       string
	bindPassword string

	baseDN       string
	scope        int
	searchFilter string
	getFilter    string

	entryIDMapping   []string
	attributeMapping ldapAttributeMapping

	logger    logrus.FieldLogger
	dialer    *net.Dialer
	tlsConfig *tls.Config

	timeout int
	limiter *rate.Limiter
}

type ldapAttributeMapping map[string]string

var ldapDefaultAttributeMapping = ldapAttributeMapping{
	ldapDefinitions.AttributeLogin:                        ldapDefinitions.AttributeLogin,
	ldapDefinitions.AttributeEmail:                        ldapDefinitions.AttributeEmail,
	ldapDefinitions.AttributeName:                         ldapDefinitions.AttributeName,
	ldapDefinitions.AttributeFamilyName:                   ldapDefinitions.AttributeFamilyName,
	ldapDefinitions.AttributeGivenName:                    ldapDefinitions.AttributeGivenName,
	ldapDefinitions.AttributeUUID:                         ldapDefinitions.AttributeUUID,
	fmt.Sprintf("%s_type", ldapDefinitions.AttributeUUID): ldapDefinitions.AttributeValueTypeText,
}

func (m ldapAttributeMapping) attributes() []string {
	attributes := make([]string, len(m)+1)
	attributes[0] = ldapDefinitions.AttributeDN
	idx := 1
	for _, attribute := range m {
		attributes[idx] = attribute
		idx++
	}

	return attributes
}

type ldapUser struct {
	entryID string
	data    ldapAttributeMapping
}

func newLdapUser(entryID string, mapping ldapAttributeMapping, entry *ldap.Entry) *ldapUser {
	// Go through all returned attributes, add them to the local data set if
	// we know them in the mapping.
	data := make(ldapAttributeMapping)
	for _, attribute := range entry.Attributes {
		if len(attribute.Values) == 0 {
			continue
		}
		for n, mapped := range mapping {
			// LDAP attribute descriptors / short names are case insensitive. See
			// https://tools.ietf.org/html/rfc4512#page-4.
			if strings.ToLower(attribute.Name) == strings.ToLower(mapped) {
				// Check if we need conversion.
				switch mapping[fmt.Sprintf("%s_type", n)] {
				case ldapDefinitions.AttributeValueTypeBinary:
					// Binary gets encoded witih Base64.
					data[n] = base64.StdEncoding.EncodeToString(attribute.ByteValues[0])
				case ldapDefinitions.AttributeValueTypeUUID:
					// Try to decode as UUID https://tools.ietf.org/html/rfc4122 and
					// serialize to string.
					if value, err := uuid.FromBytes(attribute.ByteValues[0]); err == nil {
						data[n] = value.String()
					}
				default:
					data[n] = attribute.Values[0]
				}
			}
		}
	}

	return &ldapUser{
		entryID: entryID,
		data:    data,
	}
}

func (u *ldapUser) getAttributeValue(n string) string {
	if n == "" {
		return ""
	}

	return u.data[n]
}

func (u *ldapUser) Subject() string {
	return u.entryID
}

func (u *ldapUser) Email() string {
	return u.getAttributeValue(ldapDefinitions.AttributeEmail)
}

func (u *ldapUser) EmailVerified() bool {
	return false
}

func (u *ldapUser) Name() string {
	return u.getAttributeValue(ldapDefinitions.AttributeName)
}

func (u *ldapUser) FamilyName() string {
	return u.getAttributeValue(ldapDefinitions.AttributeFamilyName)
}

func (u *ldapUser) GivenName() string {
	return u.getAttributeValue(ldapDefinitions.AttributeGivenName)
}

func (u *ldapUser) Username() string {
	return u.getAttributeValue(ldapDefinitions.AttributeLogin)
}

func (u *ldapUser) UniqueID() string {
	return u.getAttributeValue(ldapDefinitions.AttributeUUID)
}

func (u *ldapUser) BackendClaims() map[string]interface{} {
	claims := make(map[string]interface{})
	claims[konnect.IdentifiedUserIDClaim] = u.entryID

	return claims
}

// NewLDAPIdentifierBackend creates a new LDAPIdentifierBackend with the provided
// parameters.
func NewLDAPIdentifierBackend(
	c *config.Config,
	tlsConfig *tls.Config,
	uriString,
	bindDN,
	bindPassword,
	baseDN,
	scopeString,
	filter string,
	subAttributes []string,
	mappedAttributes map[string]string,
) (*LDAPIdentifierBackend, error) {
	var err error
	var scope int
	var uri *url.URL
	for {
		if uriString == "" {
			err = fmt.Errorf("server must not be empty")
			break
		}
		uri, err = url.Parse(uriString)
		if err != nil {
			break
		}

		if bindDN == "" && bindPassword != "" {
			err = fmt.Errorf("bind DN must not be empty when bind password is given")
			break
		}
		if baseDN == "" {
			err = fmt.Errorf("base DN must not be empty")
			break
		}
		switch scopeString {
		case "sub":
			scope = ldap.ScopeWholeSubtree
		case "one":
			scope = ldap.ScopeSingleLevel
		case "base":
			scope = ldap.ScopeBaseObject
		case "":
			scope = ldap.ScopeWholeSubtree
		default:
			err = fmt.Errorf("unknown scope value: %v, must be one of sub, one or base", scopeString)
		}
		if err != nil {
			break
		}

		break
	}
	if err != nil {
		return nil, fmt.Errorf("ldap identifier backend %v", err)
	}

	attributeMapping := ldapAttributeMapping{}
	for k, v := range ldapDefaultAttributeMapping {
		if mapped, ok := mappedAttributes[k]; ok && mapped != "" {
			v = mapped
		}
		attributeMapping[k] = v
		c.Logger.WithField("attribute", fmt.Sprintf("%v:%v", k, v)).Debugln("ldap identifier backend set attribute")
	}

	if filter == "" {
		filter = "(objectClass=inetOrgPerson)"
	}
	c.Logger.WithField("filter", filter).Debugln("ldap identifier backend set filter")

	loginAttribute := attributeMapping[ldapDefinitions.AttributeLogin]

	addr := uri.Host
	isTLS := false

	switch uri.Scheme {
	case "":
		uri.Scheme = "ldap"
		fallthrough
	case "ldap":
		if uri.Port() == "" {
			addr += ":389"
		}
	case "ldaps":
		if uri.Port() == "" {
			addr += ":636"
		}
		isTLS = true
	default:
		err = fmt.Errorf("invalid URI scheme: %v", uri.Scheme)
	}
	if err != nil {
		return nil, fmt.Errorf("ldap identifier backend %v", err)
	}

	var entryIDMapping []string
	if len(subAttributes) > 0 {
		entryIDMapping = subAttributes
		c.Logger.WithField("mapping", entryIDMapping).Debugln("ldap identifier sub is mapped")
	}

	b := &LDAPIdentifierBackend{
		addr:         addr,
		isTLS:        isTLS,
		bindDN:       bindDN,
		bindPassword: bindPassword,
		baseDN:       baseDN,
		scope:        scope,
		searchFilter: fmt.Sprintf("(&(%s)(%s=%%s))", filter, loginAttribute),
		getFilter:    filter,

		entryIDMapping:   entryIDMapping,
		attributeMapping: attributeMapping,

		logger: c.Logger,
		dialer: &net.Dialer{
			Timeout:   ldap.DefaultTimeout,
			DualStack: true,
		},
		tlsConfig: tlsConfig,

		timeout: 60,                        //XXX(longsleep): make timeout configuration.
		limiter: rate.NewLimiter(100, 200), //XXX(longsleep): make rate limits configuration.
	}

	b.logger.WithField("ldap", fmt.Sprintf("%s://%s ", uri.Scheme, addr)).Infoln("ldap server identifier backend set up")

	return b, nil
}

// RunWithContext implements the Backend interface.
func (b *LDAPIdentifierBackend) RunWithContext(ctx context.Context) error {
	return nil
}

// Logon implements the Backend interface, enabling Logon with user name and
// password as provided. Requests are bound to the provided context.
func (b *LDAPIdentifierBackend) Logon(ctx context.Context, audience, username, password string) (bool, *string, *string, map[string]interface{}, error) {
	loginAttributeName := b.attributeMapping[ldapDefinitions.AttributeLogin]
	if loginAttributeName == "" {
		return false, nil, nil, nil, fmt.Errorf("ldap identifier backend logon impossible as no login attribute is set")
	}

	l, err := b.connect(ctx)
	if err != nil {
		return false, nil, nil, nil, fmt.Errorf("ldap identifier backend logon connect error: %v", err)
	}
	defer l.Close()

	// Search for the given username.
	entry, err := b.searchUsername(l, username, b.attributeMapping.attributes())
	switch {
	case ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject):
		return false, nil, nil, nil, nil
	}
	if err != nil {
		return false, nil, nil, nil, fmt.Errorf("ldap identifier backend logon search error: %v", err)
	}
	if !strings.EqualFold(entry.GetAttributeValue(loginAttributeName), username) {
		return false, nil, nil, nil, fmt.Errorf("ldap identifier backend logon search returned wrong user")
	}

	// Bind as the user to verify the password.
	err = l.Bind(entry.DN, password)
	switch {
	case ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials):
		return false, nil, nil, nil, nil
	}

	if err != nil {
		return false, nil, nil, nil, fmt.Errorf("ldap identifier backend logon error: %v", err)
	}

	entryID := b.entryIDFromEntry(b.attributeMapping, entry)
	if entryID == "" {
		return false, nil, nil, nil, fmt.Errorf("ldap identifier backend logon entry without entry ID: %v", entry.DN)
	}

	user := newLdapUser(entryID, b.attributeMapping, entry)
	b.logger.WithFields(logrus.Fields{
		"username": username,
		"id":       entryID,
	}).Debugln("ldap identifier backend logon")

	return true, &entryID, nil, user.BackendClaims(), nil
}

// ResolveUserByUsername implements the Beckend interface, providing lookup for
// user by providing the username. Requests are bound to the provided context.
func (b *LDAPIdentifierBackend) ResolveUserByUsername(ctx context.Context, username string) (UserFromBackend, error) {
	loginAttributeName := b.attributeMapping[ldapDefinitions.AttributeLogin]
	if loginAttributeName == "" {
		return nil, fmt.Errorf("ldap identifier backend resolve impossible as no login attribute is set")
	}

	l, err := b.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("ldap identifier backend resolve connect error: %v", err)
	}
	defer l.Close()

	// Search for the given username.
	entry, err := b.searchUsername(l, username, b.attributeMapping.attributes())
	switch {
	case ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject):
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ldap identifier backend resolve search error: %v", err)
	}
	if !strings.EqualFold(entry.GetAttributeValue(loginAttributeName), username) {
		return nil, fmt.Errorf("ldap identifier backend resolve search returned wrong user")
	}

	return newLdapUser(entry.DN, b.attributeMapping, entry), nil
}

// GetUser implements the Backend interface, providing user meta data retrieval
// for the user specified by the userID. Requests are bound to the provided
// context.
func (b *LDAPIdentifierBackend) GetUser(ctx context.Context, entryID string, sessionRef *string) (UserFromBackend, error) {
	l, err := b.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("ldap identifier backend get user connect error: %v", err)
	}
	defer l.Close()

	entry, err := b.getUser(l, entryID, b.attributeMapping.attributes())
	if err != nil {
		return nil, fmt.Errorf("ldap identifier backend get user error: %v", err)
	}

	newEntryID := b.entryIDFromEntry(b.attributeMapping, entry)
	if !strings.EqualFold(newEntryID, entryID) {
		return nil, fmt.Errorf("ldap identifier backend get user returned wrong user")
	}

	return newLdapUser(newEntryID, b.attributeMapping, entry), nil
}

// RefreshSession implements the Backend interface.
func (b *LDAPIdentifierBackend) RefreshSession(ctx context.Context, userID string, sessionRef *string, claims map[string]interface{}) error {
	return nil
}

// DestroySession implements the Backend interface providing destroy to KC session.
func (b *LDAPIdentifierBackend) DestroySession(ctx context.Context, sessionRef *string) error {
	return nil
}

// UserClaims implements the Backend interface, providing user specific claims
// for the user specified by the userID.
func (b *LDAPIdentifierBackend) UserClaims(userID string, authorizedScopes map[string]bool) map[string]interface{} {
	return nil
}

// ScopesSupported implements the Backend interface, providing supported scopes
// when running this backend.
func (b *LDAPIdentifierBackend) ScopesSupported() []string {
	return ldapSupportedScopes
}

// ScopesMeta implements the Backend interface, providing meta data for
// supported scopes.
func (b *LDAPIdentifierBackend) ScopesMeta() *scopes.Scopes {
	return nil
}

// Name implements the Backend interface.
func (b *LDAPIdentifierBackend) Name() string {
	return ldapIdentifierBackendName
}

func (b *LDAPIdentifierBackend) connect(parentCtx context.Context) (*ldap.Conn, error) {
	// A timeout for waiting for a limiter slot. The timeout also includes the
	// time to connect to the LDAP server which as a consequence means that both
	// getting a free slot and establishing the connection are one timeout.
	ctx, cancel := context.WithTimeout(parentCtx, time.Duration(b.timeout)*time.Second)
	defer cancel()

	err := b.limiter.Wait(ctx)
	if err != nil {
		return nil, err
	}

	c, err := b.dialer.DialContext(ctx, "tcp", b.addr)
	if err != nil {
		return nil, ldap.NewError(ldap.ErrorNetwork, err)
	}

	var l *ldap.Conn
	if b.isTLS {
		sc := tls.Client(c, b.tlsConfig)
		err = sc.Handshake()
		if err != nil {
			c.Close()
			return nil, ldap.NewError(ldap.ErrorNetwork, err)
		}
		l = ldap.NewConn(sc, true)

	} else {
		l = ldap.NewConn(c, false)
	}

	l.Start()

	// Bind with general user (which is preferably read only).
	if b.bindDN != "" {
		err = l.Bind(b.bindDN, b.bindPassword)
		if err != nil {
			return nil, err
		}
	}

	return l, nil
}

func (b *LDAPIdentifierBackend) searchUsername(l *ldap.Conn, username string, attributes []string) (*ldap.Entry, error) {
	base, filter := b.baseAndSearchFilterFromUsername(username)
	// Search for the given username.
	searchRequest := ldap.NewSearchRequest(
		base,
		b.scope, ldap.NeverDerefAliases, 1, b.timeout, false,
		filter,
		attributes,
		nil,
	)
	sr, err := l.Search(searchRequest)
	if err != nil {
		return nil, err
	}

	switch len(sr.Entries) {
	case 0:
		// Nothing found.
		return nil, ldap.NewError(ldap.LDAPResultNoSuchObject, err)
	case 1:
		// Exactly one found, success.
		return sr.Entries[0], nil
	default:
		// Invalid when multiple matched.
		return nil, fmt.Errorf("user too many entries returned")
	}
}

func (b *LDAPIdentifierBackend) getUser(l *ldap.Conn, entryID string, attributes []string) (*ldap.Entry, error) {
	base, filter := b.baseAndGetFilterFromEntryID(entryID)
	if base == "" || filter == "" || entryID == "" {
		return nil, fmt.Errorf("ldap identifier backend get user invalid user ID: %v", entryID)
	}

	scope := b.scope
	if base == entryID {
		// Ensure that scope is limited, when directly requesting an entry.
		scope = ldap.ScopeBaseObject
	}

	// search for the given DN.
	searchRequest := ldap.NewSearchRequest(
		base,
		scope, ldap.NeverDerefAliases, 1, b.timeout, false,
		filter,
		attributes,
		nil,
	)
	sr, err := l.Search(searchRequest)
	if err != nil {
		return nil, err
	}
	if len(sr.Entries) != 1 {
		return nil, fmt.Errorf("user does not exist or too many entries returned")
	}

	return sr.Entries[0], nil
}

func (b *LDAPIdentifierBackend) entryIDFromEntry(mapping ldapAttributeMapping, entry *ldap.Entry) string {
	if b.entryIDMapping != nil {
		// Encode as URL query.
		values := url.Values{}
		for _, k := range b.entryIDMapping {
			v := entry.GetAttributeValues(k)
			if len(v) > 0 {
				values[k] = v
			}
		}
		// URL encode values to string.
		return values.Encode()
	}

	// Use DN by default is no mapping is set.
	return entry.DN
}

func (b *LDAPIdentifierBackend) baseAndGetFilterFromEntryID(entryID string) (string, string) {
	if b.entryIDMapping != nil {
		// Parse entryID as URL encoded query values, and build & filter to search for them all.
		if values, err := url.ParseQuery(entryID); err == nil {
			filter := ""
			for k, values := range values {
				for _, value := range values {
					filter = fmt.Sprintf("%s(%s=%s)", filter, k, value)
				}
			}
			if filter != "" {
				return b.baseDN, fmt.Sprintf("(&%s%s)", b.getFilter, filter)
			}
		}
		// Failed to parse entry ID.
		return "", ""
	}

	// Map DN to entryID.
	_, err := ldap.ParseDN(entryID)
	if err != nil {
		return "", ""
	}
	return entryID, b.getFilter
}

func (b *LDAPIdentifierBackend) baseAndSearchFilterFromUsername(username string) (string, string) {
	// Build search filter with username.
	return b.baseDN, fmt.Sprintf(b.searchFilter, username)
}
