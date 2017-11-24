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

package identifier

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sirupsen/logrus"

	"stash.kopano.io/kc/konnect/oidc"
	"stash.kopano.io/kc/konnect/utils"
)

func (i *Identifier) staticHandler(handler http.Handler, cache bool) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		addCommonResponseHeaders(rw.Header())
		if cache {
			rw.Header().Set("Cache-Control", "max-age=3153600, public")
		} else {
			rw.Header().Set("Cache-Control", "no-cache, max-age=0, public")
		}
		if strings.HasSuffix(req.URL.Path, "/") {
			// Do not serve folder-ish resources.
			i.ErrorPage(rw, http.StatusNotFound, "", "")
			return
		}
		handler.ServeHTTP(rw, req)
	})
}

func (i *Identifier) secureHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		var err error

		// TODO(longsleep): Add support for X-Forwareded-Host with trusted proxy.
		// NOTE: this does not protect from DNS rebinding. Protection for that
		// should be added at the frontend proxy.
		requiredHost := req.Host

		// This follows https://www.owasp.org/index.php/Cross-Site_Request_Forgery_(CSRF)_Prevention_Cheat_Sheet
		for {
			if req.Header.Get("Kopano-Konnect-XSRF") != "1" {
				err = fmt.Errorf("missing xsrf header")
				break
			}

			origin := req.Header.Get("Origin")
			referer := req.Header.Get("Referer")

			// Require either Origin and Referer header.
			// NOTE(longsleep): Firefox does not send Origin header for POST
			// requests when on the same domain - this is fuck (tm). See
			// https://bugzilla.mozilla.org/show_bug.cgi?id=446344 for reference.
			if origin == "" && referer == "" {
				err = fmt.Errorf("missing origin or referer header")
				break
			}

			if origin != "" {
				originURL, urlParseErr := url.Parse(origin)
				if urlParseErr != nil {
					err = fmt.Errorf("invalid origin value: %v", urlParseErr)
					break
				}
				if originURL.Host != requiredHost {
					err = fmt.Errorf("origin does not match request URL")
					break
				}
			} else if referer != "" {
				refererURL, urlParseErr := url.Parse(referer)
				if urlParseErr != nil {
					err = fmt.Errorf("invalid referer value: %v", urlParseErr)
					break
				}
				if refererURL.Host != requiredHost {
					err = fmt.Errorf("referer does not match request URL")
					break
				}
			} else {
				i.logger.WithFields(logrus.Fields{
					"host":       requiredHost,
					"user-agent": req.UserAgent(),
				}).Warn("identifier HTTP request is insecure with no Origin and Referer")
			}

			handler.ServeHTTP(rw, req)
			return
		}

		if err != nil {
			i.logger.WithError(err).WithFields(logrus.Fields{
				"host":       requiredHost,
				"referer":    req.Referer(),
				"user-agent": req.UserAgent(),
				"origin":     req.Header.Get("Origin"),
			}).Warn("rejecting identifier HTTP request")
		}

		i.ErrorPage(rw, http.StatusBadRequest, "", "")
	})
}

func (i *Identifier) handleLogon(rw http.ResponseWriter, req *http.Request) {
	decoder := json.NewDecoder(req.Body)
	var r LogonRequest
	err := decoder.Decode(&r)
	if err != nil {
		i.logger.WithError(err).Debugln("identifier failed to decode logon request")
		i.ErrorPage(rw, http.StatusBadRequest, "", "failed to decode request JSON")
		return
	}

	var user *IdentifiedUser
	response := &LogonResponse{
		State: r.State,
	}

	addNoCacheResponseHeaders(rw.Header())

	params := r.Params
	// Params is an array like this [$username, $password, $mode].
	for {
		if len(params) >= 3 && params[1] == "" && params[2] == "1" {
			// Check if same user is logged in via cookie.
			identifiedUser, cookieErr := i.GetUserFromLogonCookie(req.Context(), req)
			if cookieErr != nil {
				i.logger.WithError(cookieErr).Debugln("identifier failed to decode logon cookie in logon request")
			}
			if identifiedUser != nil {
				if identifiedUser.Username() == params[0] {
					user = identifiedUser
					break
				}
			}
		}

		forwardedUser := req.Header.Get("X-Forwarded-User")
		if forwardedUser != "" {
			// Check frontend proxy injected auth (Eg. Kerberos/NTLM).
			if len(params) >= 1 && forwardedUser == params[0] {
				user, err = i.resolveUser(req.Context(), params[0])
				if err != nil {
					i.logger.WithError(err).Errorln("identifier failed to resolve user with backend")
					i.ErrorPage(rw, http.StatusInternalServerError, "", "failed to resolve user")
					return
				}
			}
			break
		}

		if len(params) >= 2 && params[1] == "" {
			// Empty password, stop here.
			break
		}

		if len(params) >= 3 && params[2] == "1" {
			// Username and password.
			var success bool
			var subject *string
			success, subject, err = i.backend.Logon(req.Context(), params[0], params[1])
			if err != nil {
				i.logger.WithError(err).Errorln("identifier failed to logon with backend")
				i.ErrorPage(rw, http.StatusInternalServerError, "", "failed to logon")
				return
			}
			if success {
				// Construct user from logon result.
				user = &IdentifiedUser{
					sub:      *subject,
					username: params[0],
				}
			}
			break
		}

		break
	}

	if user == nil || user.Subject() == "" {
		rw.Header().Set("Kopano-Konnect-State", response.State)
		rw.WriteHeader(http.StatusNoContent)
		return
	}

	// Get user meta data.
	// TODO(longsleep): This is an additional request to the backend. This
	// should be avoided. Best would be if the backend would return everything
	// in one shot (TODO in core).
	err = i.updateUser(req.Context(), user)
	if err != nil {
		i.logger.WithError(err).Debugln("identifier failed to update user data in logon request")
	}

	if r.Hello != nil {
		err = r.Hello.parse()
		if err != nil {
			i.logger.WithError(err).Debugln("identifier failed to parse logon request hello")
			i.ErrorPage(rw, http.StatusBadRequest, "", "failed to parse request values")
			return
		}

		hello, errHello := i.newHelloResponse(rw, req, r.Hello, user)
		if errHello != nil {
			i.logger.WithError(errHello).Debugln("rejecting identifier logon request")
			i.ErrorPage(rw, http.StatusBadRequest, "", errHello.Error())
			return
		}
		if !hello.Success {
			rw.Header().Set("Kopano-Konnect-State", response.State)
			rw.WriteHeader(http.StatusNoContent)
			return
		}

		response.Hello = hello
	}

	err = i.setLogonCookie(rw, user)
	if err != nil {
		i.logger.WithError(err).Errorln("failed to serialize logon ticket")
		i.ErrorPage(rw, http.StatusInternalServerError, "", "failed to serialize logon ticket")
		return
	}

	response.Success = true

	err = utils.WriteJSON(rw, http.StatusOK, response, "")
	if err != nil {
		i.logger.WithError(err).Errorln("logon request failed writing response")
	}
}

func (i *Identifier) handleLogoff(rw http.ResponseWriter, req *http.Request) {
	decoder := json.NewDecoder(req.Body)
	var r StateRequest
	err := decoder.Decode(&r)
	if err != nil {
		i.logger.WithError(err).Debugln("identifier failed to decode logoff request")
		i.ErrorPage(rw, http.StatusBadRequest, "", "failed to decode request JSON")
		return
	}

	addNoCacheResponseHeaders(rw.Header())

	err = i.removeLogonCookie(rw)
	if err != nil {
		i.logger.WithError(err).Errorln("identifier failed to set logoff ticket")
		i.ErrorPage(rw, http.StatusInternalServerError, "", "failed to set logoff ticket")
		return
	}

	response := &StateResponse{
		State:   r.State,
		Success: true,
	}

	err = utils.WriteJSON(rw, http.StatusOK, response, "")
	if err != nil {
		i.logger.WithError(err).Errorln("logoff request failed writing response")
	}
}

func (i *Identifier) handleConsent(rw http.ResponseWriter, req *http.Request) {
	decoder := json.NewDecoder(req.Body)
	var r ConsentRequest
	err := decoder.Decode(&r)
	if err != nil {
		i.logger.WithError(err).Debugln("identifier failed to decode consent request")
		i.ErrorPage(rw, http.StatusBadRequest, "", "failed to decode request JSON")
		return
	}

	addNoCacheResponseHeaders(rw.Header())

	if !r.Allow {
		rw.Header().Set("Kopano-Konnect-State", r.State)
		rw.WriteHeader(http.StatusNoContent)
		return
	}

	err = i.setConsentCookie(rw, req, &r, &Consent{
		Allow:    r.Allow,
		RawScope: r.RawScope,
	})
	if err != nil {
		i.logger.WithError(err).Errorln("failed to serialize consent ticket")
		i.ErrorPage(rw, http.StatusInternalServerError, "", "failed to serialize consent ticket")
		return
	}

	response := &StateResponse{
		State:   r.State,
		Success: true,
	}

	err = utils.WriteJSON(rw, http.StatusOK, response, "")
	if err != nil {
		i.logger.WithError(err).Errorln("logoff request failed writing response")
	}
}

func (i *Identifier) handleHello(rw http.ResponseWriter, req *http.Request) {
	decoder := json.NewDecoder(req.Body)
	var r HelloRequest
	err := decoder.Decode(&r)
	if err != nil {
		i.logger.WithError(err).Debugln("identifier failed to decode hello request")
		i.ErrorPage(rw, http.StatusBadRequest, "", "failed to decode request JSON")
		return
	}
	err = r.parse()
	if err != nil {
		i.logger.WithError(err).Debugln("identifier failed to parse hello request")
		i.ErrorPage(rw, http.StatusBadRequest, "", "failed to parse request values")
		return
	}

	addNoCacheResponseHeaders(rw.Header())

	response, err := i.newHelloResponse(rw, req, &r, nil)
	if err != nil {
		i.logger.WithError(err).Debugln("rejecting identifier hello request")
		i.ErrorPage(rw, http.StatusBadRequest, "", err.Error())
		return
	}
	if !response.Success {
		rw.Header().Set("Kopano-Konnect-State", response.State)
		rw.WriteHeader(http.StatusNoContent)
		return
	}

	err = utils.WriteJSON(rw, http.StatusOK, response, "")
	if err != nil {
		i.logger.WithError(err).Errorln("hello request failed writing response")
	}
}

func (i Identifier) newHelloResponse(rw http.ResponseWriter, req *http.Request, r *HelloRequest, identifiedUser *IdentifiedUser) (*HelloResponse, error) {
	var err error
	response := &HelloResponse{
		State: r.State,
	}

handleHelloLoop:
	for {
		// Check prompt value.
		switch {
		case r.Prompts[oidc.PromptNone] == true:
			// Never show sign-in, directly return error.
			return nil, fmt.Errorf("prompt none requested")
		case r.Prompts[oidc.PromptLogin] == true:
			// Ignore all potential sources, when prompt login was requested.
			break handleHelloLoop
		default:
			// Let all other prompt values pass.
		}

		if identifiedUser == nil {
			// Check if logged in via cookie.
			identifiedUser, err = i.GetUserFromLogonCookie(req.Context(), req)
			if err != nil {
				i.logger.WithError(err).Debugln("identifier failed to decode logon cookie in hello")
			}
		}

		if identifiedUser != nil {
			response.Username = identifiedUser.Username()
			response.DisplayName = identifiedUser.Name()
			if response.Username != "" {
				response.Success = true
				break
			}
		}

		// Check frontend proxy injected auth (Eg. Kerberos/NTLM).
		forwardedUser := req.Header.Get("X-Forwarded-User")
		if forwardedUser != "" {
			response.Username = forwardedUser
			response.Success = true
			break
		}

		break
	}
	if !response.Success {
		return response, nil
	}

	switch r.Flow {
	case FlowOAuth:
		fallthrough
	case FlowConsent:
		fallthrough
	case FlowOIDC:
		clientDetails, err := i.clients.Lookup(req.Context(), r.ClientID, r.RedirectURI)
		if err != nil {
			return nil, err
		}

		promptConsent := false

		// Check prompt value.
		switch {
		case r.Prompts[oidc.PromptConsent] == true:
			promptConsent = true
		default:
			// Let all other prompt values pass.
		}

		// If not trusted, always force consent.
		if !clientDetails.Trusted {
			promptConsent = true
		}

		if promptConsent {
			response.Next = FlowConsent
			response.RequestedScopes = r.Scopes
			response.ClientDetails = clientDetails
		}

		// Add authorize endpoint URI as continue URI.
		response.ContinueURI = i.authorizationEndpointURI.String()
		response.Flow = r.Flow
	}

	return response, nil
}
