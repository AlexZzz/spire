package bundle

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	bundlev1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/bundle/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"github.com/spiffe/spire/pkg/common/telemetry"
	"github.com/spiffe/spire/pkg/server/api"
	"github.com/spiffe/spire/pkg/server/api/rpccontext"
	"github.com/spiffe/spire/pkg/server/cache/dscache"
	"github.com/spiffe/spire/pkg/server/plugin/datastore"
	"github.com/spiffe/spire/proto/spire/common"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UpstreamPublisher defines the publisher interface.
type UpstreamPublisher interface {
	PublishJWTKey(ctx context.Context, jwtKey *common.PublicKey) ([]*common.PublicKey, error)
}

// UpstreamPublisherFunc defines the function.
type UpstreamPublisherFunc func(ctx context.Context, jwtKey *common.PublicKey) ([]*common.PublicKey, error)

// PublishJWTKey publishes the JWT key with the given function.
func (fn UpstreamPublisherFunc) PublishJWTKey(ctx context.Context, jwtKey *common.PublicKey) ([]*common.PublicKey, error) {
	return fn(ctx, jwtKey)
}

// Config defines the bundle service configuration.
type Config struct {
	DataStore         datastore.DataStore
	TrustDomain       spiffeid.TrustDomain
	UpstreamPublisher UpstreamPublisher
}

// Service defines the v1 bundle service properties.
type Service struct {
	bundlev1.UnsafeBundleServer

	ds datastore.DataStore
	td spiffeid.TrustDomain
	up UpstreamPublisher
}

// New creates a new bundle service.
func New(config Config) *Service {
	return &Service{
		ds: config.DataStore,
		td: config.TrustDomain,
		up: config.UpstreamPublisher,
	}
}

// RegisterService registers the bundle service on the gRPC server.
func RegisterService(s *grpc.Server, service *Service) {
	bundlev1.RegisterBundleServer(s, service)
}

// CountBundles returns the total number of bundles.
func (s *Service) CountBundles(ctx context.Context, req *bundlev1.CountBundlesRequest) (*bundlev1.CountBundlesResponse, error) {
	count, err := s.ds.CountBundles(ctx)
	if err != nil {
		log := rpccontext.Logger(ctx)
		return nil, api.MakeErr(log, codes.Internal, "failed to count bundles", err)
	}

	return &bundlev1.CountBundlesResponse{Count: count}, nil
}

// GetBundle returns the bundle associated with the given trust domain.
func (s *Service) GetBundle(ctx context.Context, req *bundlev1.GetBundleRequest) (*types.Bundle, error) {
	log := rpccontext.Logger(ctx)

	dsResp, err := s.ds.FetchBundle(dscache.WithCache(ctx), &datastore.FetchBundleRequest{
		TrustDomainId: s.td.IDString(),
	})
	if err != nil {
		return nil, api.MakeErr(log, codes.Internal, "failed to fetch bundle", err)
	}

	if dsResp.Bundle == nil {
		return nil, api.MakeErr(log, codes.NotFound, "bundle not found", nil)
	}

	bundle, err := api.BundleToProto(dsResp.Bundle)
	if err != nil {
		return nil, api.MakeErr(log, codes.Internal, "failed to convert bundle", err)
	}

	applyBundleMask(bundle, req.OutputMask)
	return bundle, nil
}

// AppendBundle appends the given authorities to the given bundlev1.
func (s *Service) AppendBundle(ctx context.Context, req *bundlev1.AppendBundleRequest) (*types.Bundle, error) {
	log := rpccontext.Logger(ctx)

	if len(req.JwtAuthorities) == 0 && len(req.X509Authorities) == 0 {
		return nil, api.MakeErr(log, codes.InvalidArgument, "no authorities to append", nil)
	}

	log = log.WithField(telemetry.TrustDomainID, s.td.String())

	jwtAuth, err := api.ParseJWTAuthorities(req.JwtAuthorities)
	if err != nil {
		return nil, api.MakeErr(log, codes.InvalidArgument, "failed to convert JWT authority", err)
	}

	x509Auth, err := api.ParseX509Authorities(req.X509Authorities)
	if err != nil {
		return nil, api.MakeErr(log, codes.InvalidArgument, "failed to convert X.509 authority", err)
	}

	resp, err := s.ds.AppendBundle(ctx, &datastore.AppendBundleRequest{
		Bundle: &common.Bundle{
			TrustDomainId:  s.td.IDString(),
			JwtSigningKeys: jwtAuth,
			RootCas:        x509Auth,
		},
	})
	if err != nil {
		return nil, api.MakeErr(log, codes.Internal, "failed to append bundle", err)
	}

	bundle, err := api.BundleToProto(resp.Bundle)
	if err != nil {
		return nil, api.MakeErr(log, codes.Internal, "failed to convert bundle", err)
	}

	applyBundleMask(bundle, req.OutputMask)
	return bundle, nil
}

// PublishJWTAuthority published the JWT key on the server.
func (s *Service) PublishJWTAuthority(ctx context.Context, req *bundlev1.PublishJWTAuthorityRequest) (*bundlev1.PublishJWTAuthorityResponse, error) {
	log := rpccontext.Logger(ctx)

	if err := rpccontext.RateLimit(ctx, 1); err != nil {
		return nil, api.MakeErr(log, status.Code(err), "rejecting request due to key publishing rate limiting", err)
	}

	if req.JwtAuthority == nil {
		return nil, api.MakeErr(log, codes.InvalidArgument, "missing JWT authority", nil)
	}

	keys, err := api.ParseJWTAuthorities([]*types.JWTKey{req.JwtAuthority})
	if err != nil {
		return nil, api.MakeErr(log, codes.InvalidArgument, "invalid JWT authority", err)
	}

	resp, err := s.up.PublishJWTKey(ctx, keys[0])
	if err != nil {
		return nil, api.MakeErr(log, codes.Internal, "failed to publish JWT key", err)
	}

	return &bundlev1.PublishJWTAuthorityResponse{
		JwtAuthorities: api.PublicKeysToProto(resp),
	}, nil
}

// ListFederatedBundles returns an optionally paginated list of federated bundles.
func (s *Service) ListFederatedBundles(ctx context.Context, req *bundlev1.ListFederatedBundlesRequest) (*bundlev1.ListFederatedBundlesResponse, error) {
	log := rpccontext.Logger(ctx)

	listReq := &datastore.ListBundlesRequest{}

	// Set pagination parameters
	if req.PageSize > 0 {
		listReq.Pagination = &datastore.Pagination{
			PageSize: req.PageSize,
			Token:    req.PageToken,
		}
	}

	dsResp, err := s.ds.ListBundles(ctx, listReq)
	if err != nil {
		return nil, api.MakeErr(log, codes.Internal, "failed to list bundles", err)
	}

	resp := &bundlev1.ListFederatedBundlesResponse{}

	if dsResp.Pagination != nil {
		resp.NextPageToken = dsResp.Pagination.Token
	}

	for _, dsBundle := range dsResp.Bundles {
		log = log.WithField(telemetry.TrustDomainID, dsBundle.TrustDomainId)
		td, err := spiffeid.TrustDomainFromString(dsBundle.TrustDomainId)
		if err != nil {
			return nil, api.MakeErr(log, codes.Internal, "bundle has an invalid trust domain ID", err)
		}

		// Filter server bundle
		if s.td.Compare(td) == 0 {
			continue
		}

		b, err := api.BundleToProto(dsBundle)
		if err != nil {
			return nil, api.MakeErr(log, codes.Internal, "failed to convert bundle", err)
		}
		applyBundleMask(b, req.OutputMask)
		resp.Bundles = append(resp.Bundles, b)
	}

	return resp, nil
}

// GetFederatedBundle returns the bundle associated with the given trust domain.
func (s *Service) GetFederatedBundle(ctx context.Context, req *bundlev1.GetFederatedBundleRequest) (*types.Bundle, error) {
	log := rpccontext.Logger(ctx).WithField(telemetry.TrustDomainID, req.TrustDomain)

	td, err := spiffeid.TrustDomainFromString(req.TrustDomain)
	if err != nil {
		return nil, api.MakeErr(log, codes.InvalidArgument, "trust domain argument is not valid", err)
	}

	if s.td.Compare(td) == 0 {
		return nil, api.MakeErr(log, codes.InvalidArgument, "getting a federated bundle for the server's own trust domain is not allowed", nil)
	}

	dsResp, err := s.ds.FetchBundle(ctx, &datastore.FetchBundleRequest{
		TrustDomainId: td.IDString(),
	})
	if err != nil {
		return nil, api.MakeErr(log, codes.Internal, "failed to fetch bundle", err)
	}

	if dsResp.Bundle == nil {
		return nil, api.MakeErr(log, codes.NotFound, "bundle not found", nil)
	}

	b, err := api.BundleToProto(dsResp.Bundle)
	if err != nil {
		return nil, api.MakeErr(log, codes.Internal, "failed to convert bundle", err)
	}

	applyBundleMask(b, req.OutputMask)

	return b, nil
}

// BatchCreateFederatedBundle adds one or more bundles to the server.
func (s *Service) BatchCreateFederatedBundle(ctx context.Context, req *bundlev1.BatchCreateFederatedBundleRequest) (*bundlev1.BatchCreateFederatedBundleResponse, error) {
	var results []*bundlev1.BatchCreateFederatedBundleResponse_Result
	for _, b := range req.Bundle {
		results = append(results, s.createFederatedBundle(ctx, b, req.OutputMask))
	}

	return &bundlev1.BatchCreateFederatedBundleResponse{
		Results: results,
	}, nil
}

func (s *Service) createFederatedBundle(ctx context.Context, b *types.Bundle, outputMask *types.BundleMask) *bundlev1.BatchCreateFederatedBundleResponse_Result {
	log := rpccontext.Logger(ctx).WithField(telemetry.TrustDomainID, b.TrustDomain)

	td, err := spiffeid.TrustDomainFromString(b.TrustDomain)
	if err != nil {
		return &bundlev1.BatchCreateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "trust domain argument is not valid", err),
		}
	}

	if s.td.Compare(td) == 0 {
		return &bundlev1.BatchCreateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "creating a federated bundle for the server's own trust domain is not allowed", nil),
		}
	}

	dsBundle, err := api.ProtoToBundle(b)
	if err != nil {
		return &bundlev1.BatchCreateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "failed to convert bundle", err),
		}
	}

	resp, err := s.ds.CreateBundle(ctx, &datastore.CreateBundleRequest{
		Bundle: dsBundle,
	})
	switch status.Code(err) {
	case codes.OK:
	case codes.AlreadyExists:
		return &bundlev1.BatchCreateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.AlreadyExists, "bundle already exists", nil),
		}
	default:
		return &bundlev1.BatchCreateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.Internal, "unable to create bundle", err),
		}
	}

	protoBundle, err := api.BundleToProto(resp.Bundle)
	if err != nil {
		return &bundlev1.BatchCreateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.Internal, "failed to convert bundle", err),
		}
	}

	applyBundleMask(protoBundle, outputMask)

	log.Debug("Federated bundle created")
	return &bundlev1.BatchCreateFederatedBundleResponse_Result{
		Status: api.OK(),
		Bundle: protoBundle,
	}
}

func (s *Service) setFederatedBundle(ctx context.Context, b *types.Bundle, outputMask *types.BundleMask) *bundlev1.BatchSetFederatedBundleResponse_Result {
	log := rpccontext.Logger(ctx).WithField(telemetry.TrustDomainID, b.TrustDomain)

	td, err := spiffeid.TrustDomainFromString(b.TrustDomain)
	if err != nil {
		return &bundlev1.BatchSetFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "trust domain argument is not valid", err),
		}
	}

	if s.td.Compare(td) == 0 {
		return &bundlev1.BatchSetFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "setting a federated bundle for the server's own trust domain is not allowed", nil),
		}
	}

	dsBundle, err := api.ProtoToBundle(b)
	if err != nil {
		return &bundlev1.BatchSetFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "failed to convert bundle", err),
		}
	}
	resp, err := s.ds.SetBundle(ctx, &datastore.SetBundleRequest{
		Bundle: dsBundle,
	})

	if err != nil {
		return &bundlev1.BatchSetFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.Internal, "failed to set bundle", err),
		}
	}

	protoBundle, err := api.BundleToProto(resp.Bundle)
	if err != nil {
		return &bundlev1.BatchSetFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.Internal, "failed to convert bundle", err),
		}
	}

	applyBundleMask(protoBundle, outputMask)
	log.Info("Bundle set successfully")
	return &bundlev1.BatchSetFederatedBundleResponse_Result{
		Status: api.OK(),
		Bundle: protoBundle,
	}
}

// BatchUpdateFederatedBundle updates one or more bundles in the server.
func (s *Service) BatchUpdateFederatedBundle(ctx context.Context, req *bundlev1.BatchUpdateFederatedBundleRequest) (*bundlev1.BatchUpdateFederatedBundleResponse, error) {
	var results []*bundlev1.BatchUpdateFederatedBundleResponse_Result
	for _, b := range req.Bundle {
		results = append(results, s.updateFederatedBundle(ctx, b, req.InputMask, req.OutputMask))
	}

	return &bundlev1.BatchUpdateFederatedBundleResponse{
		Results: results,
	}, nil
}

func (s *Service) updateFederatedBundle(ctx context.Context, b *types.Bundle, inputMask, outputMask *types.BundleMask) *bundlev1.BatchUpdateFederatedBundleResponse_Result {
	log := rpccontext.Logger(ctx).WithField(telemetry.TrustDomainID, b.TrustDomain)

	td, err := spiffeid.TrustDomainFromString(b.TrustDomain)
	if err != nil {
		return &bundlev1.BatchUpdateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "trust domain argument is not valid", err),
		}
	}

	if s.td.Compare(td) == 0 {
		return &bundlev1.BatchUpdateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "updating a federated bundle for the server's own trust domain is not allowed", nil),
		}
	}

	dsBundle, err := api.ProtoToBundle(b)
	if err != nil {
		return &bundlev1.BatchUpdateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.InvalidArgument, "failed to convert bundle", err),
		}
	}
	resp, err := s.ds.UpdateBundle(ctx, &datastore.UpdateBundleRequest{
		Bundle:    dsBundle,
		InputMask: api.ProtoToBundleMask(inputMask),
	})

	switch status.Code(err) {
	case codes.OK:
	case codes.NotFound:
		return &bundlev1.BatchUpdateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.NotFound, "bundle not found", err),
		}
	default:
		return &bundlev1.BatchUpdateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.Internal, "failed to update bundle", err),
		}
	}

	protoBundle, err := api.BundleToProto(resp.Bundle)
	if err != nil {
		return &bundlev1.BatchUpdateFederatedBundleResponse_Result{
			Status: api.MakeStatus(log, codes.Internal, "failed to convert bundle", err),
		}
	}

	applyBundleMask(protoBundle, outputMask)

	log.Debug("Federated bundle updated")
	return &bundlev1.BatchUpdateFederatedBundleResponse_Result{
		Status: api.OK(),
		Bundle: protoBundle,
	}
}

// BatchSetFederatedBundle upserts one or more bundles in the server.
func (s *Service) BatchSetFederatedBundle(ctx context.Context, req *bundlev1.BatchSetFederatedBundleRequest) (*bundlev1.BatchSetFederatedBundleResponse, error) {
	var results []*bundlev1.BatchSetFederatedBundleResponse_Result
	for _, b := range req.Bundle {
		results = append(results, s.setFederatedBundle(ctx, b, req.OutputMask))
	}

	return &bundlev1.BatchSetFederatedBundleResponse{
		Results: results,
	}, nil
}

// BatchDeleteFederatedBundle removes one or more bundles from the server.
func (s *Service) BatchDeleteFederatedBundle(ctx context.Context, req *bundlev1.BatchDeleteFederatedBundleRequest) (*bundlev1.BatchDeleteFederatedBundleResponse, error) {
	log := rpccontext.Logger(ctx)
	mode, err := parseDeleteMode(req.Mode)
	if err != nil {
		return nil, api.MakeErr(log, codes.InvalidArgument, "failed to parse deletion mode", err)
	}
	log = log.WithField(telemetry.DeleteFederatedBundleMode, mode.String())

	var results []*bundlev1.BatchDeleteFederatedBundleResponse_Result
	for _, trustDomain := range req.TrustDomains {
		results = append(results, s.deleteFederatedBundle(ctx, log, trustDomain, mode))
	}

	return &bundlev1.BatchDeleteFederatedBundleResponse{
		Results: results,
	}, nil
}

func (s *Service) deleteFederatedBundle(ctx context.Context, log logrus.FieldLogger, trustDomain string, mode datastore.DeleteBundleRequest_Mode) *bundlev1.BatchDeleteFederatedBundleResponse_Result {
	log = log.WithField(telemetry.TrustDomainID, trustDomain)

	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return &bundlev1.BatchDeleteFederatedBundleResponse_Result{
			Status:      api.MakeStatus(log, codes.InvalidArgument, "trust domain argument is not valid", err),
			TrustDomain: trustDomain,
		}
	}

	if s.td.Compare(td) == 0 {
		return &bundlev1.BatchDeleteFederatedBundleResponse_Result{
			TrustDomain: trustDomain,
			Status:      api.MakeStatus(log, codes.InvalidArgument, "removing the bundle for the server trust domain is not allowed", nil),
		}
	}

	_, err = s.ds.DeleteBundle(ctx, &datastore.DeleteBundleRequest{
		TrustDomainId: td.IDString(),
		Mode:          mode,
	})

	code := status.Code(err)
	switch code {
	case codes.OK:
		return &bundlev1.BatchDeleteFederatedBundleResponse_Result{
			Status:      api.OK(),
			TrustDomain: trustDomain,
		}
	case codes.NotFound:
		return &bundlev1.BatchDeleteFederatedBundleResponse_Result{
			Status:      api.MakeStatus(log, codes.NotFound, "bundle not found", err),
			TrustDomain: trustDomain,
		}
	default:
		return &bundlev1.BatchDeleteFederatedBundleResponse_Result{
			TrustDomain: trustDomain,
			Status:      api.MakeStatus(log, code, "failed to delete federated bundle", err),
		}
	}
}

func parseDeleteMode(mode bundlev1.BatchDeleteFederatedBundleRequest_Mode) (datastore.DeleteBundleRequest_Mode, error) {
	switch mode {
	case bundlev1.BatchDeleteFederatedBundleRequest_RESTRICT:
		return datastore.DeleteBundleRequest_RESTRICT, nil
	case bundlev1.BatchDeleteFederatedBundleRequest_DISSOCIATE:
		return datastore.DeleteBundleRequest_DISSOCIATE, nil
	case bundlev1.BatchDeleteFederatedBundleRequest_DELETE:
		return datastore.DeleteBundleRequest_DELETE, nil
	default:
		return datastore.DeleteBundleRequest_RESTRICT, fmt.Errorf("unhandled delete mode %q", mode)
	}
}

func applyBundleMask(b *types.Bundle, mask *types.BundleMask) {
	if mask == nil {
		return
	}

	if !mask.RefreshHint {
		b.RefreshHint = 0
	}

	if !mask.SequenceNumber {
		b.SequenceNumber = 0
	}

	if !mask.X509Authorities {
		b.X509Authorities = nil
	}

	if !mask.JwtAuthorities {
		b.JwtAuthorities = nil
	}
}
