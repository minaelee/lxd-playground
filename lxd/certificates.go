package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/mux"

	"github.com/canonical/lxd/client"
	"github.com/canonical/lxd/lxd/auth"
	"github.com/canonical/lxd/lxd/certificate"
	"github.com/canonical/lxd/lxd/cluster"
	clusterConfig "github.com/canonical/lxd/lxd/cluster/config"
	clusterRequest "github.com/canonical/lxd/lxd/cluster/request"
	"github.com/canonical/lxd/lxd/db"
	dbCluster "github.com/canonical/lxd/lxd/db/cluster"
	"github.com/canonical/lxd/lxd/db/operationtype"
	"github.com/canonical/lxd/lxd/entity"
	"github.com/canonical/lxd/lxd/lifecycle"
	"github.com/canonical/lxd/lxd/operations"
	"github.com/canonical/lxd/lxd/request"
	"github.com/canonical/lxd/lxd/response"
	"github.com/canonical/lxd/lxd/state"
	"github.com/canonical/lxd/lxd/util"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/logger"
	"github.com/canonical/lxd/shared/version"
)

var certificatesCmd = APIEndpoint{
	Path: "certificates",

	Get:  APIEndpointAction{Handler: certificatesGet, AccessHandler: allowAuthenticated},
	Post: APIEndpointAction{Handler: certificatesPost, AllowUntrusted: true},
}

var certificateCmd = APIEndpoint{
	Path: "certificates/{fingerprint}",

	Delete: APIEndpointAction{Handler: certificateDelete, AccessHandler: allowAuthenticated},
	Get:    APIEndpointAction{Handler: certificateGet, AccessHandler: allowPermission(entity.TypeCertificate, auth.EntitlementCanView, "fingerprint")},
	Patch:  APIEndpointAction{Handler: certificatePatch, AccessHandler: allowAuthenticated},
	Put:    APIEndpointAction{Handler: certificatePut, AccessHandler: allowAuthenticated},
}

// swagger:operation GET /1.0/certificates certificates certificates_get
//
//  Get the trusted certificates
//
//  Returns a list of trusted certificates (URLs).
//
//  ---
//  produces:
//    - application/json
//  responses:
//    "200":
//      description: API endpoints
//      schema:
//        type: object
//        description: Sync response
//        properties:
//          type:
//            type: string
//            description: Response type
//            example: sync
//          status:
//            type: string
//            description: Status description
//            example: Success
//          status_code:
//            type: integer
//            description: Status code
//            example: 200
//          metadata:
//            type: array
//            description: List of endpoints
//            items:
//              type: string
//            example: |-
//              [
//                "/1.0/certificates/390fdd27ed5dc2408edc11fe602eafceb6c025ddbad9341dfdcb1056a8dd98b1",
//                "/1.0/certificates/22aee3f051f96abe6d7756892eecabf4b4b22e2ba877840a4ca981e9ea54030a"
//              ]
//    "403":
//      $ref: "#/responses/Forbidden"
//    "500":
//      $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/certificates?recursion=1 certificates certificates_get_recursion1
//
//	Get the trusted certificates
//
//	Returns a list of trusted certificates (structs).
//
//	---
//	produces:
//	  - application/json
//	responses:
//	  "200":
//	    description: API endpoints
//	    schema:
//	      type: object
//	      description: Sync response
//	      properties:
//	        type:
//	          type: string
//	          description: Response type
//	          example: sync
//	        status:
//	          type: string
//	          description: Status description
//	          example: Success
//	        status_code:
//	          type: integer
//	          description: Status code
//	          example: 200
//	        metadata:
//	          type: array
//	          description: List of certificates
//	          items:
//	            $ref: "#/definitions/Certificate"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func certificatesGet(d *Daemon, r *http.Request) response.Response {
	recursion := util.IsRecursionRequest(r)
	s := d.State()

	userHasPermission, err := s.Authorizer.GetPermissionChecker(r.Context(), r, auth.EntitlementCanView, entity.TypeCertificate)
	if err != nil {
		return response.SmartError(err)
	}

	if recursion {
		var certResponses []api.Certificate
		var baseCerts []dbCluster.Certificate
		var err error
		err = d.State().DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			baseCerts, err = dbCluster.GetCertificates(ctx, tx.Tx())
			if err != nil {
				return err
			}

			certResponses = make([]api.Certificate, 0, len(baseCerts))
			for _, baseCert := range baseCerts {
				if !userHasPermission(entity.CertificateURL(baseCert.Fingerprint)) {
					continue
				}

				apiCert, err := baseCert.ToAPI(ctx, tx.Tx())
				if err != nil {
					return err
				}

				certResponses = append(certResponses, *apiCert)
			}

			return nil
		})
		if err != nil {
			return response.SmartError(err)
		}

		return response.SyncResponse(true, certResponses)
	}

	body := []string{}
	for _, identity := range d.identityCache.GetByAuthenticationMethod(api.AuthenticationMethodTLS) {
		if !userHasPermission(entity.CertificateURL(identity.Identifier)) {
			continue
		}

		certificateURL := fmt.Sprintf("/%s/certificates/%s", version.APIVersion, identity.Identifier)
		body = append(body, certificateURL)
	}

	return response.SyncResponse(true, body)
}

// clusterMemberJoinTokenValid searches for cluster join token that matches the join token provided.
// Returns matching operation if found and cancels the operation, otherwise returns nil.
func clusterMemberJoinTokenValid(s *state.State, r *http.Request, projectName string, joinToken *api.ClusterMemberJoinToken) (*api.Operation, error) {
	ops, err := operationsGetByType(s, r, projectName, operationtype.ClusterJoinToken)
	if err != nil {
		return nil, fmt.Errorf("Failed getting cluster join token operations: %w", err)
	}

	var foundOp *api.Operation
	for _, op := range ops {
		if op.StatusCode != api.Running {
			continue // Tokens are single use, so if cancelled but not deleted yet its not available.
		}

		if op.Resources == nil {
			continue
		}

		opSecret, ok := op.Metadata["secret"]
		if !ok {
			continue
		}

		opServerName, ok := op.Metadata["serverName"]
		if !ok {
			continue
		}

		if opServerName == joinToken.ServerName && opSecret == joinToken.Secret {
			foundOp = op
			break
		}
	}

	if foundOp != nil {
		// Token is single-use, so cancel it now.
		err = operationCancel(s, r, projectName, foundOp)
		if err != nil {
			return nil, fmt.Errorf("Failed to cancel operation %q: %w", foundOp.ID, err)
		}

		expiresAt, ok := foundOp.Metadata["expiresAt"]
		if ok {
			var expiry time.Time

			// Depending on whether it's a local operation or not, expiry will either be a time.Time or a string.
			if s.ServerName == foundOp.Location {
				expiry, _ = expiresAt.(time.Time)
			} else {
				expiry, _ = time.Parse(time.RFC3339Nano, expiresAt.(string))
			}

			// Check if token has expired.
			if time.Now().After(expiry) {
				return nil, api.StatusErrorf(http.StatusForbidden, "Token has expired")
			}
		}

		return foundOp, nil
	}

	// No operation found.
	return nil, nil
}

// certificateTokenValid searches for certificate token that matches the add token provided.
// Returns matching operation if found and cancels the operation, otherwise returns nil.
func certificateTokenValid(s *state.State, r *http.Request, addToken *api.CertificateAddToken) (*api.Operation, error) {
	ops, err := operationsGetByType(s, r, api.ProjectDefaultName, operationtype.CertificateAddToken)
	if err != nil {
		return nil, fmt.Errorf("Failed getting certificate token operations: %w", err)
	}

	var foundOp *api.Operation
	for _, op := range ops {
		if op.StatusCode != api.Running {
			continue // Tokens are single use, so if cancelled but not deleted yet its not available.
		}

		opSecret, ok := op.Metadata["secret"]
		if !ok {
			continue
		}

		if opSecret == addToken.Secret {
			foundOp = op
			break
		}
	}

	if foundOp != nil {
		// Token is single-use, so cancel it now.
		err = operationCancel(s, r, api.ProjectDefaultName, foundOp)
		if err != nil {
			return nil, fmt.Errorf("Failed to cancel operation %q: %w", foundOp.ID, err)
		}

		expiresAt, ok := foundOp.Metadata["expiresAt"]
		if ok {
			expiry, _ := expiresAt.(time.Time)

			// Check if token has expired.
			if time.Now().After(expiry) {
				return nil, api.StatusErrorf(http.StatusForbidden, "Token has expired")
			}
		}

		return foundOp, nil
	}

	// No operation found.
	return nil, nil
}

// swagger:operation POST /1.0/certificates?public certificates certificates_post_untrusted
//
//  Add a trusted certificate
//
//  Adds a certificate to the trust store as an untrusted user.
//  In this mode, the `password` property must be set to the correct value.
//
//  The `certificate` field can be omitted in which case the TLS client
//  certificate in use for the connection will be retrieved and added to the
//  trust store.
//
//  The `?public` part of the URL isn't required, it's simply used to
//  separate the two behaviors of this endpoint.
//
//  ---
//  consumes:
//    - application/json
//  produces:
//    - application/json
//  parameters:
//    - in: body
//      name: certificate
//      description: Certificate
//      required: true
//      schema:
//        $ref: "#/definitions/CertificatesPost"
//  responses:
//    "200":
//      $ref: "#/responses/EmptySyncResponse"
//    "400":
//      $ref: "#/responses/BadRequest"
//    "403":
//      $ref: "#/responses/Forbidden"
//    "500":
//      $ref: "#/responses/InternalServerError"

// swagger:operation POST /1.0/certificates certificates certificates_post
//
//	Add a trusted certificate
//
//	Adds a certificate to the trust store.
//	In this mode, the `password` property is always ignored.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: certificate
//	    description: Certificate
//	    required: true
//	    schema:
//	      $ref: "#/definitions/CertificatesPost"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func certificatesPost(d *Daemon, r *http.Request) response.Response {
	s := d.State()

	// Parse the request.
	req := api.CertificatesPost{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	localHTTPSAddress := s.LocalConfig.HTTPSAddress()

	// Quick check.
	if req.Token && req.Certificate != "" {
		return response.BadRequest(fmt.Errorf("Can't use certificate if token is requested"))
	}

	if req.Token {
		if req.Type != "client" {
			return response.BadRequest(fmt.Errorf("Tokens can only be issued for client certificates"))
		}

		if localHTTPSAddress == "" {
			return response.BadRequest(fmt.Errorf("Can't issue token when server isn't listening on network"))
		}
	}

	// Access check.
	// Can't us s.GlobalConfig.TrustPassword() here as global config is not yet updated.
	var secret string
	err = s.DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
		config, err := clusterConfig.Load(ctx, tx)
		if err != nil {
			return err
		}

		secret = config.TrustPassword()

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Check if the user is already trusted.
	trusted, _, _, _, err := d.Authenticate(nil, r)
	if err != nil {
		return response.SmartError(err)
	}

	// User isn't an admin and is already trusted, can't add more certs.
	if trusted && req.Certificate == "" && !req.Token {
		return response.BadRequest(fmt.Errorf("Client is already trusted"))
	}

	// Handle requests by non-admin users.
	var userCanCreateCertificates bool
	err = s.Authorizer.CheckPermission(r.Context(), r, entity.ServerURL(), auth.EntitlementCanCreateIdentities)
	if err == nil {
		userCanCreateCertificates = true
	} else if !api.StatusErrorCheck(err, http.StatusForbidden) {
		return response.SmartError(err)
	}

	if !trusted || !userCanCreateCertificates {
		// Non-admin cannot issue tokens.
		if req.Token {
			return response.Forbidden(nil)
		}

		// A password is required for non-admin users.
		if req.Password == "" {
			return response.Forbidden(nil)
		}

		// Check if cluster member join token supplied as password.
		joinToken, err := shared.JoinTokenDecode(req.Password)
		if err == nil {
			// If so then check there is a matching join operation.
			joinOp, err := clusterMemberJoinTokenValid(s, r, api.ProjectDefaultName, joinToken)
			if err != nil {
				return response.InternalError(fmt.Errorf("Failed during search for join token operation: %w", err))
			}

			if joinOp == nil {
				return response.Forbidden(fmt.Errorf("No matching cluster join operation found"))
			}
		} else {
			// Check if certificate add token supplied as password.
			joinToken, err := shared.CertificateTokenDecode(req.Password)
			if err == nil {
				// If so then check there is a matching join operation.
				joinOp, err := certificateTokenValid(s, r, joinToken)
				if err != nil {
					return response.InternalError(fmt.Errorf("Failed during search for certificate add token operation: %w", err))
				}

				if joinOp == nil {
					return response.Forbidden(fmt.Errorf("No matching certificate add operation found"))
				}

				tokenReq, ok := joinOp.Metadata["request"].(api.CertificatesPost)
				if !ok {
					return response.InternalError(fmt.Errorf("Bad certificate add operation data"))
				}

				// Create a new request from the token data as the user isn't allowed to override anything.
				req = api.CertificatesPost{
					CertificatePut: api.CertificatePut{
						Name:       tokenReq.Name,
						Type:       tokenReq.Type,
						Restricted: tokenReq.Restricted,
						Projects:   tokenReq.Projects,
					},
				}
			} else {
				// Otherwise check if password matches trust password.
				if util.PasswordCheck(secret, req.Password) != nil {
					logger.Warn("Bad trust password", logger.Ctx{"url": r.URL.RequestURI(), "ip": r.RemoteAddr})
					return response.Forbidden(nil)
				}
			}
		}
	}

	dbReqType, err := certificate.FromAPIType(req.Type)
	if err != nil {
		return response.BadRequest(err)
	}

	// Extract the certificate.
	var cert *x509.Certificate
	if req.Certificate != "" {
		// Add supplied certificate.
		data, err := base64.StdEncoding.DecodeString(req.Certificate)
		if err != nil {
			return response.BadRequest(err)
		}

		cert, err = x509.ParseCertificate(data)
		if err != nil {
			return response.BadRequest(fmt.Errorf("Invalid certificate material: %w", err))
		}
	} else if req.Token {
		// Get all addresses the server is listening on. This is encoded in the certificate token,
		// so that the client will not have to specify a server address. The client will iterate
		// through all these addresses until it can connect to one of them.
		addresses, err := util.ListenAddresses(localHTTPSAddress)
		if err != nil {
			return response.InternalError(err)
		}

		// Generate join secret for new client. This will be stored inside the join token operation and will be
		// supplied by the joining client (encoded inside the join token) which will allow us to lookup the correct
		// operation in order to validate the requested joining client name is correct and authorised.
		joinSecret, err := shared.RandomCryptoString()
		if err != nil {
			return response.InternalError(err)
		}

		// Generate fingerprint of network certificate so joining member can automatically trust the correct
		// certificate when it is presented during the join process.
		fingerprint, err := shared.CertFingerprintStr(string(s.Endpoints.NetworkPublicKey()))
		if err != nil {
			return response.InternalError(err)
		}

		if req.Projects == nil {
			req.Projects = []string{}
		}

		meta := map[string]any{
			"secret":      joinSecret,
			"fingerprint": fingerprint,
			"addresses":   addresses,
			"request":     req,
		}

		// If tokens should expire, add the expiry date to the op's metadata.
		expiry := s.GlobalConfig.RemoteTokenExpiry()

		if expiry != "" {
			expiresAt, err := shared.GetExpiry(time.Now(), expiry)
			if err != nil {
				return response.InternalError(err)
			}

			meta["expiresAt"] = expiresAt
		}

		op, err := operations.OperationCreate(s, api.ProjectDefaultName, operations.OperationClassToken, operationtype.CertificateAddToken, nil, meta, nil, nil, nil, r)
		if err != nil {
			return response.InternalError(err)
		}

		return operations.OperationResponse(op)
	} else if r.TLS != nil {
		// Add client's certificate.
		if len(r.TLS.PeerCertificates) < 1 {
			// This can happen if the client doesn't send a client certificate or if the server is in
			// CA mode. We rely on this check to prevent non-CA trusted client certificates from being
			// added when in CA mode.
			return response.BadRequest(fmt.Errorf("No client certificate provided"))
		}

		cert = r.TLS.PeerCertificates[len(r.TLS.PeerCertificates)-1]
	} else {
		return response.BadRequest(fmt.Errorf("Can't use TLS data on non-TLS link"))
	}

	// Check validity.
	err = certificateValidate(cert)
	if err != nil {
		return response.BadRequest(err)
	}

	// Calculate the fingerprint.
	fingerprint := shared.CertFingerprint(cert)

	// Figure out a name.
	name := req.Name
	if name == "" {
		// Try to pull the CN.
		name = cert.Subject.CommonName
		if name == "" {
			// Fallback to the client's IP address.
			remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				return response.InternalError(err)
			}

			name = remoteHost
		}
	}

	if !isClusterNotification(r) {
		err := s.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Check if we already have the certificate.
			existingCert, _ := dbCluster.GetCertificateByFingerprintPrefix(ctx, tx.Tx(), fingerprint)
			if existingCert != nil {
				return api.StatusErrorf(http.StatusConflict, "Certificate already in trust store")
			}

			// Store the certificate in the cluster database.
			dbCert := dbCluster.Certificate{
				Fingerprint: shared.CertFingerprint(cert),
				Type:        dbReqType,
				Name:        name,
				Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})),
				Restricted:  req.Restricted,
			}

			_, err := dbCluster.CreateCertificateWithProjects(ctx, tx.Tx(), dbCert, req.Projects)
			return err
		})
		if err != nil {
			return response.SmartError(err)
		}

		// Notify other nodes about the new certificate.
		notifier, err := cluster.NewNotifier(s, s.Endpoints.NetworkCert(), s.ServerCert(), cluster.NotifyAlive)
		if err != nil {
			return response.SmartError(err)
		}

		req := api.CertificatesPost{
			CertificatePut: api.CertificatePut{
				Certificate: base64.StdEncoding.EncodeToString(cert.Raw),
				Name:        name,
				Type:        api.CertificateTypeClient,
			},
		}

		err = notifier(func(client lxd.InstanceServer) error {
			return client.CreateCertificate(req)
		})
		if err != nil {
			return response.SmartError(err)
		}

		// Add the certificate resource to the authorizer.
		err = s.Authorizer.AddCertificate(r.Context(), fingerprint)
		if err != nil {
			logger.Error("Failed to add certificate to authorizer", logger.Ctx{"fingerprint": fingerprint, "error": err})
		}
	}

	// Reload the cache.
	s.UpdateIdentityCache()

	lc := lifecycle.CertificateCreated.Event(fingerprint, request.CreateRequestor(r), nil)
	s.Events.SendLifecycle(api.ProjectDefaultName, lc)

	return response.SyncResponseLocation(true, nil, lc.Source)
}

// swagger:operation GET /1.0/certificates/{fingerprint} certificates certificate_get
//
//	Get the trusted certificate
//
//	Gets a specific certificate entry from the trust store.
//
//	---
//	produces:
//	  - application/json
//	responses:
//	  "200":
//	    description: Certificate
//	    schema:
//	      type: object
//	      description: Sync response
//	      properties:
//	        type:
//	          type: string
//	          description: Response type
//	          example: sync
//	        status:
//	          type: string
//	          description: Status description
//	          example: Success
//	        status_code:
//	          type: integer
//	          description: Status code
//	          example: 200
//	        metadata:
//	          $ref: "#/definitions/Certificate"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func certificateGet(d *Daemon, r *http.Request) response.Response {
	fingerprint, err := url.PathUnescape(mux.Vars(r)["fingerprint"])
	if err != nil {
		return response.SmartError(err)
	}

	var cert *api.Certificate
	err = d.State().DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		dbCertInfo, err := dbCluster.GetCertificateByFingerprintPrefix(ctx, tx.Tx(), fingerprint)
		if err != nil {
			return err
		}

		cert, err = dbCertInfo.ToAPI(ctx, tx.Tx())
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponseETag(true, cert, cert)
}

// swagger:operation PUT /1.0/certificates/{fingerprint} certificates certificate_put
//
//	Update the trusted certificate
//
//	Updates the entire certificate configuration.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: certificate
//	    description: Certificate configuration
//	    required: true
//	    schema:
//	      $ref: "#/definitions/CertificatePut"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "412":
//	    $ref: "#/responses/PreconditionFailed"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func certificatePut(d *Daemon, r *http.Request) response.Response {
	fingerprint, err := url.PathUnescape(mux.Vars(r)["fingerprint"])
	if err != nil {
		return response.SmartError(err)
	}

	// Get current database record.
	var apiEntry *api.Certificate
	err = d.State().DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		oldEntry, err := dbCluster.GetCertificateByFingerprintPrefix(ctx, tx.Tx(), fingerprint)
		if err != nil {
			return err
		}

		apiEntry, err = oldEntry.ToAPI(ctx, tx.Tx())
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Validate the ETag.
	err = util.EtagCheck(r, apiEntry)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Parse the request.
	req := api.CertificatePut{}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	clientType := clusterRequest.UserAgentClientType(r.Header.Get("User-Agent"))

	// Apply the update.
	return doCertificateUpdate(d, *apiEntry, req, clientType, r)
}

// swagger:operation PATCH /1.0/certificates/{fingerprint} certificates certificate_patch
//
//	Partially update the trusted certificate
//
//	Updates a subset of the certificate configuration.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: certificate
//	    description: Certificate configuration
//	    required: true
//	    schema:
//	      $ref: "#/definitions/CertificatePut"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "412":
//	    $ref: "#/responses/PreconditionFailed"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func certificatePatch(d *Daemon, r *http.Request) response.Response {
	fingerprint, err := url.PathUnescape(mux.Vars(r)["fingerprint"])
	if err != nil {
		return response.SmartError(err)
	}

	// Get current database record.
	var apiEntry *api.Certificate
	err = d.State().DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		oldEntry, err := dbCluster.GetCertificateByFingerprintPrefix(ctx, tx.Tx(), fingerprint)
		if err != nil {
			return err
		}

		apiEntry, err = oldEntry.ToAPI(ctx, tx.Tx())
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Validate the ETag.
	err = util.EtagCheck(r, apiEntry)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Apply the changes.
	req := *apiEntry
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	clientType := clusterRequest.UserAgentClientType(r.Header.Get("User-Agent"))

	return doCertificateUpdate(d, *apiEntry, req.Writable(), clientType, r)
}

func doCertificateUpdate(d *Daemon, dbInfo api.Certificate, req api.CertificatePut, clientType clusterRequest.ClientType, r *http.Request) response.Response {
	s := d.State()

	if clientType == clusterRequest.ClientTypeNormal {
		reqDBType, err := certificate.FromAPIType(req.Type)
		if err != nil {
			return response.BadRequest(err)
		}

		// Convert to the database type.
		dbCert := dbCluster.Certificate{
			Certificate: dbInfo.Certificate,
			Fingerprint: dbInfo.Fingerprint,
			Restricted:  req.Restricted,
			Name:        req.Name,
			Type:        reqDBType,
		}

		var userCanEditCertificate bool
		err = s.Authorizer.CheckPermission(r.Context(), r, entity.CertificateURL(dbInfo.Fingerprint), auth.EntitlementCanEdit)
		if err == nil {
			userCanEditCertificate = true
		} else if !api.StatusErrorCheck(err, http.StatusForbidden) {
			return response.SmartError(err)
		}

		// Non-admins are able to change their own certificate but no other fields.
		// In order to prevent possible future security issues, the certificate information is
		// reset in case a non-admin user is performing the update.
		certProjects := req.Projects
		if !userCanEditCertificate {
			if r.TLS == nil {
				response.Forbidden(fmt.Errorf("Cannot update certificate information"))
			}

			// Ensure the user in not trying to change fields other than the certificate.
			if dbInfo.Restricted != req.Restricted || dbInfo.Name != req.Name || len(dbInfo.Projects) != len(req.Projects) {
				return response.Forbidden(fmt.Errorf("Only the certificate can be changed"))
			}

			for i := 0; i < len(dbInfo.Projects); i++ {
				if dbInfo.Projects[i] != req.Projects[i] {
					return response.Forbidden(fmt.Errorf("Only the certificate can be changed"))
				}
			}

			// Reset dbCert in order to prevent possible future security issues.
			dbCert = dbCluster.Certificate{
				Certificate: dbInfo.Certificate,
				Fingerprint: dbInfo.Fingerprint,
				Restricted:  dbInfo.Restricted,
				Name:        dbInfo.Name,
				Type:        reqDBType,
			}

			certProjects = dbInfo.Projects

			if req.Certificate != "" && dbInfo.Certificate != req.Certificate {
				certBlock, _ := pem.Decode([]byte(dbInfo.Certificate))

				oldCert, err := x509.ParseCertificate(certBlock.Bytes)
				if err != nil {
					// This should not happen
					return response.InternalError(err)
				}

				trustedCerts := map[string]x509.Certificate{
					dbInfo.Name: *oldCert,
				}

				trusted := false
				for _, i := range r.TLS.PeerCertificates {
					trusted, _ = util.CheckTrustState(*i, trustedCerts, s.Endpoints.NetworkCert(), false)

					if trusted {
						break
					}
				}

				if !trusted {
					return response.Forbidden(fmt.Errorf("Certificate cannot be changed"))
				}
			}
		}

		if req.Certificate != "" && dbInfo.Certificate != req.Certificate {
			// Add supplied certificate.
			block, _ := pem.Decode([]byte(req.Certificate))

			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return response.BadRequest(fmt.Errorf("Invalid certificate material: %w", err))
			}

			dbCert.Certificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
			dbCert.Fingerprint = shared.CertFingerprint(cert)

			// Check validity.
			err = certificateValidate(cert)
			if err != nil {
				return response.BadRequest(err)
			}
		}

		// Update the database record.
		err = s.DB.UpdateCertificate(context.Background(), dbInfo.Fingerprint, dbCert, certProjects)
		if err != nil {
			return response.SmartError(err)
		}

		// Notify other nodes about the new certificate.
		notifier, err := cluster.NewNotifier(s, s.Endpoints.NetworkCert(), s.ServerCert(), cluster.NotifyAlive)
		if err != nil {
			return response.SmartError(err)
		}

		err = notifier(func(client lxd.InstanceServer) error {
			return client.UpdateCertificate(dbCert.Fingerprint, req, "")
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	// Reload the cache.
	s.UpdateIdentityCache()

	s.Events.SendLifecycle(api.ProjectDefaultName, lifecycle.CertificateUpdated.Event(dbInfo.Fingerprint, request.CreateRequestor(r), nil))

	return response.EmptySyncResponse
}

// swagger:operation DELETE /1.0/certificates/{fingerprint} certificates certificate_delete
//
//	Delete the trusted certificate
//
//	Removes the certificate from the trust store.
//
//	---
//	produces:
//	  - application/json
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func certificateDelete(d *Daemon, r *http.Request) response.Response {
	s := d.State()

	fingerprint, err := url.PathUnescape(mux.Vars(r)["fingerprint"])
	if err != nil {
		return response.SmartError(err)
	}

	if !isClusterNotification(r) {
		var certInfo *dbCluster.Certificate
		err := s.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Get current database record.
			var err error
			certInfo, err = dbCluster.GetCertificateByFingerprintPrefix(ctx, tx.Tx(), fingerprint)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return response.SmartError(err)
		}

		var userCanEditCertificate bool
		err = s.Authorizer.CheckPermission(r.Context(), r, entity.CertificateURL(fingerprint), auth.EntitlementCanDelete)
		if err == nil {
			userCanEditCertificate = true
		} else if api.StatusErrorCheck(err, http.StatusForbidden) {
			return response.SmartError(err)
		}

		// Non-admins are able to delete only their own certificate.
		if !userCanEditCertificate {
			if r.TLS == nil {
				response.Forbidden(fmt.Errorf("Cannot delete certificate"))
			}

			certBlock, _ := pem.Decode([]byte(certInfo.Certificate))

			cert, err := x509.ParseCertificate(certBlock.Bytes)
			if err != nil {
				// This should not happen
				return response.InternalError(err)
			}

			trustedCerts := map[string]x509.Certificate{
				certInfo.Name: *cert,
			}

			trusted := false
			for _, i := range r.TLS.PeerCertificates {
				trusted, _ = util.CheckTrustState(*i, trustedCerts, s.Endpoints.NetworkCert(), false)

				if trusted {
					break
				}
			}

			if !trusted {
				return response.Forbidden(fmt.Errorf("Certificate cannot be deleted"))
			}
		}

		err = s.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Perform the delete with the expanded fingerprint.
			return dbCluster.DeleteCertificate(ctx, tx.Tx(), certInfo.Fingerprint)
		})
		if err != nil {
			return response.SmartError(err)
		}

		// Notify other nodes about the new certificate.
		notifier, err := cluster.NewNotifier(s, s.Endpoints.NetworkCert(), s.ServerCert(), cluster.NotifyAlive)
		if err != nil {
			return response.SmartError(err)
		}

		err = notifier(func(client lxd.InstanceServer) error {
			return client.DeleteCertificate(certInfo.Fingerprint)
		})
		if err != nil {
			return response.SmartError(err)
		}

		// Remove the certificate from the authorizer.
		err = s.Authorizer.DeleteCertificate(r.Context(), certInfo.Fingerprint)
		if err != nil {
			logger.Error("Failed to remove certificate from authorizer", logger.Ctx{"fingerprint": certInfo.Fingerprint, "error": err})
		}
	}

	// Reload the cache.
	s.UpdateIdentityCache()

	s.Events.SendLifecycle(api.ProjectDefaultName, lifecycle.CertificateDeleted.Event(fingerprint, request.CreateRequestor(r), nil))

	return response.EmptySyncResponse
}

func certificateValidate(cert *x509.Certificate) error {
	if time.Now().Before(cert.NotBefore) {
		return fmt.Errorf("The provided certificate isn't valid yet")
	}

	if time.Now().After(cert.NotAfter) {
		return fmt.Errorf("The provided certificate is expired")
	}

	if cert.PublicKeyAlgorithm == x509.RSA {
		pubKey, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("Unable to validate the RSA certificate")
		}

		// Check that we're dealing with at least 2048bit (Size returns a value in bytes).
		if pubKey.Size()*8 < 2048 {
			return fmt.Errorf("RSA key is too weak (minimum of 2048bit)")
		}
	}

	return nil
}
