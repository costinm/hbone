// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gcp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// From nodeagent/plugin/providers/google/stsclient
// In Istio, the code is used if "GoogleCA" is set as CA_PROVIDER or CA_ADDR has the right prefix
var (
	// SecureTokenEndpoint is the Endpoint the STS client calls to.
	SecureTokenEndpoint = "https://sts.googleapis.com/v1/token"

	httpTimeout         = time.Second * 5
	contentType         = "application/json"
	Scope               = "https://www.googleapis.com/auth/cloud-platform"
	accessTokenEndpoint = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken"
	idTokenEndpoint     = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateIdToken"

	// Server side
	// TokenPath is url path for handling STS requests.
	TokenPath = "/token"
	// StsStatusPath is the path for dumping STS status.
	StsStatusPath = "/stsStatus"
	// URLEncodedForm is the encoding type specified in a STS request.
	URLEncodedForm = "application/x-www-form-urlencoded"
	// TokenExchangeGrantType is the required value for "grant_type" parameter in a STS request.
	TokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"
	// SubjectTokenType is the required token type in a STS request.
	SubjectTokenType = "urn:ietf:params:oauth:token-type:jwt"

	Debug = false
)

// error code sent in a STS error response. A full list of error code is
// defined in https://tools.ietf.org/html/rfc6749#section-5.2.
const (
	// If the request itself is not valid or if either the "subject_token" or
	// "actor_token" are invalid or unacceptable, the STS server must set
	// error code to "invalid_request".
	invalidRequest = "invalid_request"
	// If the authorization server is unwilling or unable to issue a token, the
	// STS server should set error code to "invalid_target".
	invalidTarget      = "invalid_target"
	stsIssuedTokenType = "urn:ietf:params:oauth:token-type:access_token"
)

// AuthConfig contains the settings for getting tokens using K8S or federated tokens.
type AuthConfig struct {
	// ProjectNumber is required - this code doesn't look it up.
	// Set as x-goog-user-project
	ProjectNumber string

	// TrustDomain to use - typically based on project name.
	TrustDomain string

	// GKE Cluster address.
	// https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s
	// It is also the iss field in the token.
	ClusterAddress string

	// TokenSource returns K8S or federated tokens with a given audience.
	TokenSource TokenSource
}

type TokenSource interface {
	// GetToken for a given audience.
	GetToken(context.Context, string) (string, error)
}

// STS provides token exchanges. Implements grpc and golang.org/x/oauth2.TokenSource
// The source of trust is the K8S token with TrustDomain audience, it is exchanged with access or ID tokens.
type STS struct {
	httpClient *http.Client
	kr         *AuthConfig

	// Google service account to impersonate and return tokens for.
	// The KSA returned from K8S must have the IAM permissions
	GSA string

	AudOverride string

	// K8S returns a token signed by k8s, no further exchanges.
	K8S bool

	// UseSTSExchange will return a token for an external service account.
	UseSTSExchange bool

	// UseAccessToken will force returning a GSA access token, regardless of audience.
	UseAccessToken bool
}

// NewGSATokenSource returns a oauth2.TokenSource and
// grpc credentials.PerRPCCredentials implmentation, returning access
// tokens for a Google Service Account.
//
// If the gsa is empty, the ASM mesh P4SA will be used instead. This is
// suitable for connecting to stackdriver and out-of-cluster managed Istiod.
// Otherwise, the gsa must grant the KSA (kubernetes service account)
// permission to act as the GSA.
func NewGSATokenSource(kr *AuthConfig, gsa string) *STS {
	sts, _ := NewSTS(kr)
	sts.UseSTSExchange = true
	sts.GSA = gsa
	sts.UseAccessToken = true
	return sts
}

// NewK8STokenSource returns a oauth2 and grpc token source that returns
// K8S signed JWTs with the given audience.
func NewK8STokenSource(kr *AuthConfig, audOverride string) *STS {
	sts, _ := NewSTS(kr)
	sts.K8S = true
	sts.AudOverride = audOverride
	return sts
}

// NewFederatedTokenSource returns federated tokens - google access tokens
// associated with the federated (k8s) identity. Can be used in some but not
// all APIs - in particular MeshCA requires this token.
func NewFederatedTokenSource(kr *AuthConfig) *STS {
	sts, _ := NewSTS(kr)
	sts.K8S = false
	sts.UseSTSExchange = false
	return sts
}

func NewSTS(kr *AuthConfig) (*STS, error) {
	caCertPool, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}

	return &STS{
		kr: kr,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs: caCertPool,
				},
			},
		},
	}, nil
}

//// Implements oauth2.TokenSource - returning access tokens
//// May return federated token or service account tokens
//func (s *STS) Token() (*oauth2.Token, error) {
//	mv, err := s.GetRequestMetadata(context.Background())
//	if err != nil {
//		return nil, err
//	}
//	a := mv["authorization"]
//	// WIP - split, etc
//	t := &oauth2.Token{
//		AccessToken: a,
//	}
//	return t, nil
//}

func (s *STS) md(t string) map[string]string {
	res := map[string]string{
		"authorization": "Bearer " + t,
	}
	if s.kr.ProjectNumber != "" {
		// Breaks MeshCA
		//res["x-goog-user-project"] = s.kr.ProjectNumber
	}
	return res
}

// GetRequestMetadata implements credentials.PerRPCCredentials
// This can be used for both ID tokens or access tokens - if the 'aud' containts googleapis.com, access tokens are returned.
func (s *STS) GetRequestMetadata(ctx context.Context, aud ...string) (map[string]string, error) {
	ta := ""
	if len(aud) > 0 {
		ta = aud[0]
	}
	if len(aud) > 1 {
		return nil, errors.New("Single audience supporte")
	}
	t, err := s.GetToken(ctx, ta)
	if err != nil {
		return nil, err
	}
	return s.md(t), nil
}

func (s *STS) GetToken(ctx context.Context, aud string) (string, error) {
	// The K8S-signed JWT
	kaud := s.kr.TrustDomain
	if s.K8S {
		if s.AudOverride != "" {
			kaud = s.AudOverride
		} else {
			kaud = aud
		}
	}

	kt, err := s.kr.TokenSource.GetToken(ctx, kaud)
	if err != nil {
		return "", err
	}

	if s.K8S {
		return kt, err
	}

	// Federated token - a google token equivalent with the k8s JWT, using STS
	ft, err := s.TokenFederated(ctx, kt)
	if err != nil {
		return "", err
	}

	a0 := aud

	if !s.UseSTSExchange {
		return ft, nil
	}

	token, err := s.TokenAccess(ctx, ft, a0)
	return token, err
}

func (s *STS) RequireTransportSecurity() bool {
	return false
}

// TokenFederated exchanges the K8S JWT with a federated token
// (formerly called ExchangeToken)
func (s *STS) TokenFederated(ctx context.Context, k8sSAjwt string) (string, error) {
	if s.kr.ClusterAddress == "" {
		// First time - construct it from the K8S token
		payload := TokenPayload(k8sSAjwt)
		j := &JWT{}
		_ = json.Unmarshal([]byte(payload), j)
		s.kr.ClusterAddress = j.Iss
	}
	stsAud := s.constructAudience(s.kr.TrustDomain)

	jsonStr, err := s.constructFederatedTokenRequest(stsAud, k8sSAjwt)
	if err != nil {
		return "", fmt.Errorf("failed to marshal federated token request: %v", err)
	}

	req, err := http.NewRequest("POST", SecureTokenEndpoint, bytes.NewBuffer(jsonStr))
	req = req.WithContext(ctx)

	res, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange failed: %v, (aud: %s, STS endpoint: %s)", err, stsAud, SecureTokenEndpoint)
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("token exchange read failed: %v, (aud: %s, STS endpoint: %s)", err, stsAud, SecureTokenEndpoint)
	}
	respData := &federatedTokenResponse{}
	if err := json.Unmarshal(body, respData); err != nil {
		// Normally the request should json - extremely hard to debug otherwise, not enough info in status/err
		log.Println("Unexpected unmarshal error, response was ", string(body))
		return "", fmt.Errorf("(aud: %s, STS endpoint: %s), failed to unmarshal response data of size %v: %v",
			stsAud, SecureTokenEndpoint, len(body), err)
	}

	if respData.AccessToken == "" {
		return "", fmt.Errorf(
			"exchanged empty token (aud: %s, STS endpoint: %s), response: %v", stsAud, SecureTokenEndpoint, string(body))
	}

	return respData.AccessToken, nil
}

// Exchange a federated token equivalent with the k8s JWT with the ASM p4SA.
// TODO: can be used with any GSA, if the permission to call generateAccessToken is granted.
// This is a good way to get access tokens for a GSA using the KSA, similar with TokenRequest in
// the other direction.
//
// May return an ID token with aud or access token.
func (s *STS) TokenAccess(ctx context.Context, federatedToken string, audience string) (string, error) {
	accessToken := isAccessToken(audience, s)
	req, err := s.constructGenerateAccessTokenRequest(federatedToken, audience, accessToken)
	if err != nil {
		return "", fmt.Errorf("failed to marshal federated token request: %v", err)
	}
	req = req.WithContext(ctx)
	res, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("token exchange failed: %v", err)
	}

	// Create an access token
	if accessToken {
		respData := &accessTokenResponse{}

		if err := json.Unmarshal(body, respData); err != nil {
			// Normally the request should json - extremely hard to debug otherwise, not enough info in status/err
			log.Println("Unexpected unmarshal error, response was ", string(body))
			return "", fmt.Errorf("failed to unmarshal response data of size %v: %v",
				len(body), err)
		}

		if respData.AccessToken == "" {
			return "", fmt.Errorf(
				"exchanged empty token, response: %v", string(body))
		}

		return respData.AccessToken, nil
	}

	// Return an ID token for the GSA
	respData := &idTokenResponse{}

	if err := json.Unmarshal(body, respData); err != nil {
		// Normally the request should json - extremely hard to debug otherwise, not enough info in status/err
		log.Println("Unexpected unmarshal error, response was ", string(body))
		return "", fmt.Errorf("failed to unmarshal response data of size %v: %v",
			len(body), err)
	}

	if respData.Token == "" {
		return "", fmt.Errorf(
			"exchanged empty token, response: %v", string(body))
	}

	return respData.Token, nil
}

func isAccessToken(audience string, s *STS) bool {
	return audience == "" || s.UseAccessToken || strings.Contains(audience, "googleapis.com")
}

type federatedTokenResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"` // Expiration time in seconds
}

// provider can be extracted from metadata server, or is set using GKE_ClusterURL
//
// For VMs, it is set as GoogleComputeEngine via CREDENTIAL_IDENTITY_PROVIDER env
// In Istio GKE it is constructed from metadata, on VM it is GKE_CLUSTER_URL or gcp_gke_cluster_url,
// format "https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s" - this also happens to be
// the 'iss' field in the token.
// According to docs, aud can be:
// iam.googleapis.com/projects/<project-number>/locations/global/workloadIdentityPools/<pool-id>/providers/<provider-id>.
// or gcloud URL
// Required when exchanging an external credential for a Google access token.
func (s *STS) constructAudience(trustDomain string) string {
	return fmt.Sprintf("identitynamespace:%s:%s", trustDomain, s.kr.ClusterAddress)
}

// fetchFederatedToken exchanges a third-party issued Json Web Token for an OAuth2.0 access token
// which asserts a third-party identity within an identity namespace.
func (s *STS) constructFederatedTokenRequest(aud, jwt string) ([]byte, error) {
	values := map[string]string{
		"grantType":          "urn:ietf:params:oauth:grant-type:token-exchange", // fixed, no options
		"subjectTokenType":   "urn:ietf:params:oauth:token-type:jwt",
		"requestedTokenType": "urn:ietf:params:oauth:token-type:access_token",
		"audience":           aud, // full name if the identity provider.
		"subjectToken":       jwt,
		"scope":              Scope, // required for the GCP exchanges
	}

	// golang sts also includes:
	jsonValue, err := json.Marshal(values)
	return jsonValue, err
}

// from security/security.go

// StsRequestParameters stores all STS request attributes defined in
// https://tools.ietf.org/html/draft-ietf-oauth-token-exchange-16#section-2.1
type StsRequestParameters struct {
	// REQUIRED. The value "urn:ietf:params:oauth:grant-type:token- exchange"
	// indicates that a token exchange is being performed.
	GrantType string
	// OPTIONAL. Indicates the location of the target service or resource where
	// the client intends to use the requested security token.
	Resource string
	// OPTIONAL. The logical name of the target service where the client intends
	// to use the requested security token.
	Audience string
	// OPTIONAL. A list of space-delimited, case-sensitive strings, that allow
	// the client to specify the desired Scope of the requested security token in the
	// context of the service or Resource where the token will be used.
	Scope string
	// OPTIONAL. An identifier, for the type of the requested security token.
	RequestedTokenType string
	// REQUIRED. A security token that represents the identity of the party on
	// behalf of whom the request is being made.
	SubjectToken string
	// REQUIRED. An identifier, that indicates the type of the security token in
	// the "subject_token" parameter.
	SubjectTokenType string
	// OPTIONAL. A security token that represents the identity of the acting party.
	ActorToken string
	// An identifier, that indicates the type of the security token in the
	// "actor_token" parameter.
	ActorTokenType string
}

// From stsservice/sts.go

// StsResponseParameters stores all attributes sent as JSON in a successful STS
// response. These attributes are defined in
// https://tools.ietf.org/html/draft-ietf-oauth-token-exchange-16#section-2.2.1
type StsResponseParameters struct {
	// REQUIRED. The security token issued by the authorization server
	// in response to the token exchange request.
	AccessToken string `json:"access_token"`
	// REQUIRED. An identifier, representation of the issued security token.
	IssuedTokenType string `json:"issued_token_type"`
	// REQUIRED. A case-insensitive value specifying the method of using the access
	// token issued. It provides the client with information about how to utilize the
	// access token to access protected resources.
	TokenType string `json:"token_type"`
	// RECOMMENDED. The validity lifetime, in seconds, of the token issued by the
	// authorization server.
	ExpiresIn int64 `json:"expires_in"`
	// OPTIONAL, if the Scope of the issued security token is identical to the
	// Scope requested by the client; otherwise, REQUIRED.
	Scope string `json:"scope"`
	// OPTIONAL. A refresh token will typically not be issued when the exchange is
	// of one temporary credential (the subject_token) for a different temporary
	// credential (the issued token) for use in some other context.
	RefreshToken string `json:"refresh_token"`
}

// From tokenexchangeplugin.go
type Duration struct {
	// Signed seconds of the span of time. Must be from -315,576,000,000
	// to +315,576,000,000 inclusive. Note: these bounds are computed from:
	// 60 sec/min * 60 min/hr * 24 hr/day * 365.25 days/year * 10000 years
	Seconds int64 `json:"seconds"`
}

type accessTokenRequest struct {
	Name      string   `json:"name"` // nolint: structcheck, unused
	Delegates []string `json:"delegates"`
	Scope     []string `json:"scope"`
	LifeTime  Duration `json:"lifetime"` // nolint: structcheck, unused
}

type idTokenRequest struct {
	Audience     string   `json:"audience"` // nolint: structcheck, unused
	Delegates    []string `json:"delegates"`
	IncludeEmail bool     `json:"includeEmail"`
}

type accessTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpireTime  string `json:"expireTime"`
}

type idTokenResponse struct {
	Token string `json:"token"`
}

// constructFederatedTokenRequest returns an HTTP request for access token.
// Example of an access token request:
// POST https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/
// service-<GCP project number>@gcp-sa-meshdataplane.iam.gserviceaccount.com:generateAccessToken
// Content-Type: application/json
// Authorization: Bearer <federated token>
// {
//  "Delegates": [],
//  "Scope": [
//      https://www.googleapis.com/auth/cloud-platform
//  ],
// }
//
// This requires permission to impersonate:
// gcloud iam service-accounts add-iam-policy-binding \
//  GSA_NAME@GSA_PROJECT_ID.iam.gserviceaccount.com \
//  --role=roles/iam.workloadIdentityUser \
//  --member="serviceAccount:WORKLOAD_IDENTITY_POOL[K8S_NAMESPACE/KSA_NAME]"
//
// The p4sa is auto-setup for all authenticated users.
func (s *STS) constructGenerateAccessTokenRequest(fResp string, audience string, accessToken bool) (*http.Request, error) {
	if s.GSA == "" {
		s.GSA = s.ExternalSA()
	}
	gsa := s.GSA
	endpoint := ""
	var err error
	var jsonQuery []byte
	if accessToken {
		endpoint = fmt.Sprintf(accessTokenEndpoint, gsa)
		// Request for access token with a lifetime of 3600 seconds.
		query := accessTokenRequest{
			LifeTime: Duration{Seconds: 3600},
		}
		query.Scope = append(query.Scope, Scope)

		jsonQuery, err = json.Marshal(query)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal query for get access token request: %+v", err)
		}
	} else {
		endpoint = fmt.Sprintf(idTokenEndpoint, gsa)
		// Request for access token with a lifetime of 3600 seconds.
		query := idTokenRequest{
			IncludeEmail: true,
			Audience:     audience,
		}

		jsonQuery, err = json.Marshal(query)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal query for get access token request: %+v", err)
		}
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(jsonQuery))
	if err != nil {
		return nil, fmt.Errorf("failed to create get access token request: %+v", err)
	}
	req.Header.Add("Content-Type", contentType)
	if Debug {
		reqDump, _ := httputil.DumpRequest(req, true)
		log.Println("Prepared access token request: ", string(reqDump))
	}
	req.Header.Add("Authorization", "Bearer "+fResp) // the AccessToken
	return req, nil
}

func (s *STS) ExternalSA() string {
	if s.GSA != "" {
		return s.GSA
	}
	// If not set, default to ASM default SA.
	// This has stackdriver, TD, MCP permissions - and is available to all
	// workloads. Only works for ASM clusters.
	return "service-" + s.kr.ProjectNumber + "@gcp-sa-meshdataplane.iam.gserviceaccount.com"
}

// ServeStsRequests handles STS requests and sends exchanged token in responses.
func (s *STS) ServeStsRequests(w http.ResponseWriter, req *http.Request) {
	reqParam, validationError := s.validateStsRequest(req)
	if validationError != nil {
		// If request is invalid, the error code must be "invalid_request".
		// https://tools.ietf.org/html/draft-ietf-oauth-token-exchange-16#section-2.2.2.
		s.sendErrorResponse(w, invalidRequest, validationError)
		return
	}
	// We start with reqParam.SubjectToken - loaded from the file by the client.
	// Must be a K8S Token with right trust domain
	ft, err := s.TokenFederated(req.Context(), reqParam.SubjectToken)
	if err != nil {
		s.sendErrorResponse(w, invalidTarget, err)
		return
	}

	at, err := s.TokenAccess(req.Context(), ft, "")

	if err != nil {
		log.Printf("token manager fails to generate token: %v", err)
		// If the authorization server is unable to issue a token, the "invalid_target" error code
		// should be used in the error response.
		// https://tools.ietf.org/html/draft-ietf-oauth-token-exchange-16#section-2.2.2.
		s.sendErrorResponse(w, invalidTarget, err)
		return
	}
	s.sendSuccessfulResponse(w, s.generateSTSRespInner(at))
}

func (p *STS) generateSTSRespInner(token string) []byte {
	//exp, err := time.Parse(time.RFC3339Nano, atResp.ExpireTime)
	// Default token life time is 3600 seconds
	var expireInSec int64 = 3600
	//if err == nil {
	//	expireInSec = int64(time.Until(exp).Seconds())
	//}
	stsRespParam := StsResponseParameters{
		AccessToken:     token,
		IssuedTokenType: stsIssuedTokenType,
		TokenType:       "Bearer",
		ExpiresIn:       expireInSec,
	}
	statusJSON, _ := json.MarshalIndent(stsRespParam, "", " ")
	return statusJSON
}

// validateStsRequest validates a STS request, and extracts STS parameters from the request.
func (s *STS) validateStsRequest(req *http.Request) (StsRequestParameters, error) {
	reqParam := StsRequestParameters{}
	if req == nil {
		return reqParam, errors.New("request is nil")
	}

	//if stsServerLog.DebugEnabled() {
	//	reqDump, _ := httputil.DumpRequest(req, true)
	//	stsServerLog.Debugf("Received STS request: %s", string(reqDump))
	//}
	if req.Method != "POST" {
		return reqParam, fmt.Errorf("request method is invalid, should be POST but get %s", req.Method)
	}
	if req.Header.Get("Content-Type") != URLEncodedForm {
		return reqParam, fmt.Errorf("request content type is invalid, should be %s but get %s", URLEncodedForm,
			req.Header.Get("Content-type"))
	}
	if parseErr := req.ParseForm(); parseErr != nil {
		return reqParam, fmt.Errorf("failed to parse query from STS request: %v", parseErr)
	}
	if req.PostForm.Get("grant_type") != TokenExchangeGrantType {
		return reqParam, fmt.Errorf("request query grant_type is invalid, should be %s but get %s",
			TokenExchangeGrantType, req.PostForm.Get("grant_type"))
	}
	// Only a JWT token is accepted.
	if req.PostForm.Get("subject_token") == "" {
		return reqParam, errors.New("subject_token is empty")
	}
	if req.PostForm.Get("subject_token_type") != SubjectTokenType {
		return reqParam, fmt.Errorf("subject_token_type is invalid, should be %s but get %s",
			SubjectTokenType, req.PostForm.Get("subject_token_type"))
	}
	reqParam.GrantType = req.PostForm.Get("grant_type")
	reqParam.Resource = req.PostForm.Get("resource")
	reqParam.Audience = req.PostForm.Get("audience")
	reqParam.Scope = req.PostForm.Get("scope")
	reqParam.RequestedTokenType = req.PostForm.Get("requested_token_type")
	reqParam.SubjectToken = req.PostForm.Get("subject_token")
	reqParam.SubjectTokenType = req.PostForm.Get("subject_token_type")
	reqParam.ActorToken = req.PostForm.Get("actor_token")
	reqParam.ActorTokenType = req.PostForm.Get("actor_token_type")
	return reqParam, nil
}

// StsErrorResponse stores all Error parameters sent as JSON in a STS Error response.
// The Error parameters are defined in
// https://tools.ietf.org/html/draft-ietf-oauth-token-exchange-16#section-2.2.2.
type StsErrorResponse struct {
	// REQUIRED. A single ASCII Error code.
	Error string `json:"error"`
	// OPTIONAL. Human-readable ASCII [USASCII] text providing additional information.
	ErrorDescription string `json:"error_description"`
	// OPTIONAL. A URI identifying a human-readable web page with information
	// about the Error.
	ErrorURI string `json:"error_uri"`
}

// sendErrorResponse takes error type and error details, generates an error response and sends out.
func (s *STS) sendErrorResponse(w http.ResponseWriter, errorType string, errDetail error) {
	w.Header().Add("Content-Type", "application/json")
	if errorType == invalidRequest {
		w.WriteHeader(http.StatusBadRequest)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
	errResp := StsErrorResponse{
		Error:            errorType,
		ErrorDescription: errDetail.Error(),
	}
	if errRespJSON, err := json.MarshalIndent(errResp, "", "  "); err == nil {
		if _, err := w.Write(errRespJSON); err != nil {
			return
		}
	} else {
		log.Printf("failure in marshaling error response (%v) into JSON: %v", errResp, err)
	}
}

// sendSuccessfulResponse takes token data and generates a successful STS response, and sends out the STS response.
func (s *STS) sendSuccessfulResponse(w http.ResponseWriter, tokenData []byte) {
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(tokenData); err != nil {
		log.Printf("failure in sending STS success response: %v", err)
		return
	}
}

// JWT includes minimal field for a JWT, primarily for extracting iss for the exchange.
// This is used with K8S JWTs, which use multi-string.
//
//
type JWT struct {
	//An "aud" (Audience) claim in the token MUST include the Unicode
	//serialization of the origin (Section 6.1 of [RFC6454]) of the push
	//resource URL.  This binds the token to a specific push service and
	//ensures that the token is reusable for all push resource URLs that
	//share the same origin.
	// In K8S it is an array !
	Aud MultiString `json:"aud,omitempty"`

	//If the application server wishes to provide contact details, it MAY
	//include a "sub" (Subject) claim in the JWT.  The "sub" claim SHOULD
	//include a contact URI for the application server as either a
	//"mailto:" (email) [RFC6068] or an "https:" [RFC2818] URI.
	Sub string `json:"sub,omitempty"`

	// Max 24h
	Exp int64 `json:"exp,omitempty"`
	IAT int64 `json:"iat,omitempty"`

	// Issuer - for example kubernetes/serviceaccount.
	Iss string `json:"iss,omitempty"`

	Email string `json:"email,omitempty"`

	EmailVerified bool `json:"email_verified,omitempty"`

	K8S K8SAccountInfo `json:"kubernetes.io"`

	Name string `json:"kubernetes.io/serviceaccount/service-account.name"`

	Raw string `json:-`
}

type K8SAccountInfo struct {
	Namespace string `json:"namespace"`
}

type MultiString []string

func (ms *MultiString) MarshalJSON() ([]byte, error) {
	sa := []string(*ms)
	if len(sa) == 0 {
		return []byte{}, nil
	}
	if len(sa) == 1 {
		return json.Marshal(sa[0])
	}
	return json.Marshal(sa)
}

func (ms *MultiString) UnmarshalJSON(data []byte) error {
	if len(data) > 0 {
		switch data[0] {
		case '"':
			var s string
			if err := json.Unmarshal(data, &s); err != nil {
				return err
			}
			*ms = append(*ms, s) // multiString(s)
		case '[':
			var s []string
			if err := json.Unmarshal(data, &s); err != nil {
				return err
			}
			*ms = append(*ms, s...) // multiString(strings.Join(s, ","))
		}
	}
	return nil
}

// TokenPayload returns the decoded token. Used for logging/debugging token content, without printing the signature.
func TokenPayload(jwt string) string {
	jwtSplit := strings.Split(jwt, ".")
	if len(jwtSplit) != 3 {
		return ""
	}
	//azp,"email","exp":1629832319,"iss":"https://accounts.google.com","sub":"1118295...
	payload := jwtSplit[1]

	payloadBytes, err := base64.RawStdEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}

	return string(payloadBytes)
}
