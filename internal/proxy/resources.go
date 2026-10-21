package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/btnguyen2k/nuget-proxy/internal/upstream"
)

const (
	packageResource   = "PackageBaseAddress/3.0.0"
	auditResource     = "VulnerabilityInfo/6.7.0"
	signatureResource = "RepositorySignatures/5.0.0"
	maxIndexBytes     = 1 << 20
)

var (
	errBadGateway = errors.New("invalid upstream metadata")
	pageName      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

type resource struct {
	ID   string `json:"@id"`
	Type string `json:"@type"`
}

type serviceIndex struct {
	Version   string     `json:"version"`
	Resources []resource `json:"resources"`
}

type resources struct {
	packages   string
	audit      string
	signatures string
}

func (s *Server) readJSON(ctx context.Context, rawURL string, target any) error {
	object, err := s.get(ctx, rawURL, s.cfg.Cache.MetadataTTL)
	if err != nil {
		return err
	}
	defer s.closeObject(object)
	data, err := io.ReadAll(io.LimitReader(object, maxIndexBytes+1))
	if err != nil {
		return fmt.Errorf("read cached metadata: %w", err)
	}
	if len(data) > maxIndexBytes {
		return fmt.Errorf("%w: index exceeds 1 MiB", errBadGateway)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%w: %v", errBadGateway, err)
	}
	return nil
}

func (s *Server) discover(ctx context.Context) (resources, error) {
	var index serviceIndex
	if err := s.readJSON(ctx, upstream.IndexURL, &index); err != nil {
		return resources{}, err
	}
	if index.Version != "3.0.0" {
		return resources{}, fmt.Errorf("%w: unsupported service index version %q", errBadGateway, index.Version)
	}
	var result resources
	for _, item := range index.Resources {
		var destination *string
		switch item.Type {
		case packageResource:
			destination = &result.packages
		case auditResource:
			destination = &result.audit
		case signatureResource:
			destination = &result.signatures
		default:
			continue
		}
		if *destination != "" {
			continue
		}
		if err := upstream.ValidateURL(item.ID); err != nil {
			return resources{}, fmt.Errorf("%w: %v", errBadGateway, err)
		}
		*destination = item.ID
	}
	if result.packages == "" {
		return resources{}, fmt.Errorf("%w: PackageBaseAddress is missing", errBadGateway)
	}
	base, err := url.Parse(result.packages)
	if err != nil || base.RawQuery != "" || base.ForceQuery {
		return resources{}, fmt.Errorf("%w: invalid package base address", errBadGateway)
	}
	result.packages = strings.TrimRight(result.packages, "/") + "/"
	return result, nil
}

func (s *Server) index(ctx context.Context) (serviceIndex, error) {
	remote, err := s.discover(ctx)
	if err != nil {
		return serviceIndex{}, err
	}
	index := serviceIndex{Version: "3.0.0", Resources: []resource{
		{ID: s.publicURL("/flatcontainer/"), Type: packageResource},
	}}
	if remote.audit != "" {
		index.Resources = append(index.Resources, resource{ID: s.publicURL("/vulnerabilities/index.json"), Type: auditResource})
	}
	if remote.signatures != "" && s.https {
		index.Resources = append(index.Resources, resource{ID: s.publicURL("/signatures/index.json"), Type: signatureResource})
	}
	return index, nil
}

func (s *Server) auditIndex(ctx context.Context, rawURL string) (any, error) {
	var pages []map[string]json.RawMessage
	if err := s.readJSON(ctx, rawURL, &pages); err != nil {
		return nil, err
	}
	if len(pages) < 1 || len(pages) > 16 {
		return nil, fmt.Errorf("%w: vulnerability index must contain 1 to 16 pages", errBadGateway)
	}
	names := make(map[string]bool)
	for _, page := range pages {
		var id, name, updated string
		if err := json.Unmarshal(page["@id"], &id); err != nil {
			return nil, fmt.Errorf("%w: missing vulnerability page URL", errBadGateway)
		}
		if err := upstream.ValidateURL(id); err != nil {
			return nil, fmt.Errorf("%w: %v", errBadGateway, err)
		}
		if err := json.Unmarshal(page["@name"], &name); err != nil || !pageName.MatchString(name) || names[name] {
			return nil, fmt.Errorf("%w: invalid or duplicate vulnerability page name", errBadGateway)
		}
		names[name] = true
		if err := json.Unmarshal(page["@updated"], &updated); err != nil {
			return nil, fmt.Errorf("%w: missing vulnerability update time", errBadGateway)
		}
		if _, err := time.Parse(time.RFC3339Nano, updated); err != nil {
			return nil, fmt.Errorf("%w: invalid vulnerability update time", errBadGateway)
		}
		page["@id"], _ = json.Marshal(s.publicURL("/vulnerabilities/pages/" + s.links.sign("audit", id) + ".json"))
	}
	return pages, nil
}

func (s *Server) signaturesIndex(ctx context.Context, rawURL string) (any, error) {
	var document map[string]json.RawMessage
	if err := s.readJSON(ctx, rawURL, &document); err != nil {
		return nil, err
	}
	var allSigned *bool
	if err := json.Unmarshal(document["allRepositorySigned"], &allSigned); err != nil || allSigned == nil {
		return nil, fmt.Errorf("%w: missing allRepositorySigned", errBadGateway)
	}
	var certificates []map[string]json.RawMessage
	if err := json.Unmarshal(document["signingCertificates"], &certificates); err != nil || certificates == nil ||
		(*allSigned && len(certificates) == 0) {
		return nil, fmt.Errorf("%w: invalid signing certificates", errBadGateway)
	}
	for _, certificate := range certificates {
		var contentURL string
		if err := json.Unmarshal(certificate["contentUrl"], &contentURL); err != nil {
			return nil, fmt.Errorf("%w: missing certificate URL", errBadGateway)
		}
		if err := upstream.ValidateURL(contentURL); err != nil {
			return nil, fmt.Errorf("%w: %v", errBadGateway, err)
		}
		certificate["contentUrl"], _ = json.Marshal(s.publicURL("/signatures/certificates/" + s.links.sign("certificate", contentURL) + ".crt"))
	}
	document["signingCertificates"], _ = json.Marshal(certificates)
	return document, nil
}
