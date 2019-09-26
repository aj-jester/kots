package upstream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	imagedocker "github.com/containers/image/docker"
	imagetypes "github.com/containers/image/types"
	"github.com/docker/distribution/registry/api/errcode"
	"github.com/pkg/errors"
	kotsv1beta1 "github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
	kotsscheme "github.com/replicatedhq/kots/kotskinds/client/kotsclientset/scheme"
	"github.com/replicatedhq/kots/pkg/image"
	"github.com/replicatedhq/kots/pkg/logger"
	"github.com/replicatedhq/kots/pkg/template"
	"github.com/replicatedhq/kots/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"
	kustomizeimage "sigs.k8s.io/kustomize/v3/pkg/image"
)

const DefaultMetadata = `apiVersion: kots.io/v1beta1
kind: Application
metadata:
  name: "Application"
spec:
  title: "Application"
  icon: https://cdn1.iconfinder.com/data/icons/ninja-things-1/1772/ninja-simple-512.png`

type ReplicatedUpstream struct {
	Channel      *string
	AppSlug      string
	VersionLabel *string
	Sequence     *int
}

type App struct {
	Name string
}

type Release struct {
	UpdateCursor string
	VersionLabel string
	Manifests    map[string][]byte
}

func downloadReplicated(u *url.URL, localPath string, license *kotsv1beta1.License) (*Upstream, error) {
	var release *Release

	if localPath != "" {
		parsedLocalRelease, err := readReplicatedAppFromLocalPath(localPath)
		if err != nil {
			return nil, errors.Wrap(err, "failed to read replicated app from local path")
		}

		release = parsedLocalRelease
	} else {
		// A license file is required to be set for this to succeed
		if license == nil {
			return nil, errors.New("No license was provided")
		}

		replicatedUpstream, err := parseReplicatedURL(u)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse replicated upstream")
		}

		license, err := getSuccessfulHeadResponse(replicatedUpstream, license)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get successful head response")
		}

		downloadedRelease, err := downloadReplicatedApp(replicatedUpstream, license)
		if err != nil {
			return nil, errors.Wrap(err, "failed to download replicated app")
		}

		release = downloadedRelease
	}

	// Find the config in the upstream and write out default values
	application := findAppInRelease(release)
	config := findConfigInRelease(release)
	if config != nil {
		configValues, err := createEmptyConfigValues(application.Name, config)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create empty config values")
		}

		release.Manifests["userdata/config.yaml"] = mustMarshalConfigValues(configValues)
	}

	// Add the license to the upstream, if one was propvided
	if license != nil {
		release.Manifests["userdata/license.yaml"] = mustMarshalLicense(license)
	}

	files, err := releaseToFiles(release)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get files from release")
	}

	upstream := &Upstream{
		URI:          u.RequestURI(),
		Name:         application.Name,
		Files:        files,
		Type:         "replicated",
		UpdateCursor: release.UpdateCursor,
		VersionLabel: release.VersionLabel,
	}

	return upstream, nil
}

func (r *ReplicatedUpstream) getRequest(method string, license *kotsv1beta1.License) (*http.Request, error) {
	u, err := url.Parse(license.Spec.Endpoint)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse endpoint from license")
	}

	hostname := u.Hostname()
	if u.Port() != "" {
		hostname = fmt.Sprintf("%s:%s", u.Hostname(), u.Port())
	}

	url := fmt.Sprintf("%s://%s/release/%s", u.Scheme, hostname, license.Spec.AppSlug)

	if r.Channel != nil {
		url = fmt.Sprintf("%s/%s", url, *r.Channel)
	}

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to call newrequest")
	}

	req.Header.Set("Authorization", fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", license.Spec.LicenseID, license.Spec.LicenseID)))))

	return req, nil
}

func parseReplicatedURL(u *url.URL) (*ReplicatedUpstream, error) {
	replicatedUpstream := ReplicatedUpstream{}

	if u.User != nil {
		if u.User.Username() != "" {
			replicatedUpstream.AppSlug = u.User.Username()
			versionLabel := u.Hostname()
			replicatedUpstream.VersionLabel = &versionLabel
		}
	}

	if replicatedUpstream.AppSlug == "" {
		replicatedUpstream.AppSlug = u.Hostname()
		if u.Path != "" {
			channel := strings.TrimPrefix(u.Path, "/")
			replicatedUpstream.Channel = &channel
		}
	}

	return &replicatedUpstream, nil
}

func getSuccessfulHeadResponse(replicatedUpstream *ReplicatedUpstream, license *kotsv1beta1.License) (*kotsv1beta1.License, error) {
	headReq, err := replicatedUpstream.getRequest("HEAD", license)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create http request")
	}
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		return nil, errors.Wrap(err, "failed to execute head request")
	}

	if headResp.StatusCode == 401 {
		return nil, errors.Wrap(err, "license was not accepted")
	}

	if headResp.StatusCode >= 400 {
		return nil, errors.Errorf("expected result from head request: %d", headResp.StatusCode)
	}

	return license, nil
}

func readReplicatedAppFromLocalPath(localPath string) (*Release, error) {
	release := Release{
		Manifests:    make(map[string][]byte),
		UpdateCursor: "-1", // TODO
	}

	err := filepath.Walk(localPath,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if info.IsDir() {
				return nil
			}

			contents, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}

			// remove localpath prefix
			appPath := strings.TrimPrefix(path, localPath)
			appPath = strings.TrimLeft(appPath, string(os.PathSeparator))

			release.Manifests[appPath] = contents

			return nil
		})
	if err != nil {
		return nil, errors.Wrap(err, "failed to walk local path")
	}

	return &release, nil
}

func downloadReplicatedApp(replicatedUpstream *ReplicatedUpstream, license *kotsv1beta1.License) (*Release, error) {
	getReq, err := replicatedUpstream.getRequest("GET", license)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create http request")
	}
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		return nil, errors.Wrap(err, "failed to execute get request")
	}

	if getResp.StatusCode >= 400 {
		return nil, errors.Errorf("expected result from get request: %d", getResp.StatusCode)
	}

	defer getResp.Body.Close()

	updateCursor := getResp.Header.Get("X-Replicated-Sequence")
	versionLabel := getResp.Header.Get("X-Replicated-VersionLabel")

	gzf, err := gzip.NewReader(getResp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new gzip reader")
	}

	release := Release{
		Manifests:    make(map[string][]byte),
		UpdateCursor: updateCursor,
		VersionLabel: versionLabel,
	}
	tarReader := tar.NewReader(gzf)
	i := 0
	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "failed to get next file from reader")
		}

		name := header.Name

		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			content, err := ioutil.ReadAll(tarReader)
			if err != nil {
				return nil, errors.Wrap(err, "failed to read file from tar")
			}

			release.Manifests[name] = content
		}

		i++
	}

	return &release, nil
}

func mustMarshalLicense(license *kotsv1beta1.License) []byte {
	kotsscheme.AddToScheme(scheme.Scheme)

	s := json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)

	var b bytes.Buffer
	if err := s.Encode(license, &b); err != nil {
		panic(err)
	}

	return b.Bytes()
}

func mustMarshalConfigValues(configValues *kotsv1beta1.ConfigValues) []byte {
	kotsscheme.AddToScheme(scheme.Scheme)

	s := json.NewYAMLSerializer(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)

	var b bytes.Buffer
	if err := s.Encode(configValues, &b); err != nil {
		panic(err)
	}

	return b.Bytes()
}

func createEmptyConfigValues(applicationName string, config *kotsv1beta1.Config) (*kotsv1beta1.ConfigValues, error) {
	emptyValues := kotsv1beta1.ConfigValuesSpec{
		Values: map[string]string{},
	}

	builder := template.Builder{}
	builder.AddCtx(template.StaticCtx{})

	for _, group := range config.Spec.Groups {
		for _, item := range group.Items {
			if item.Value != "" {
				rendered, err := builder.RenderTemplate(item.Name, item.Value)
				if err != nil {
					return nil, errors.Wrap(err, "failed to render config item value")
				}

				emptyValues.Values[item.Name] = rendered
			}
		}
	}

	configValues := kotsv1beta1.ConfigValues{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kots.io/v1beta1",
			Kind:       "ConfigValues",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: applicationName,
		},
		Spec: emptyValues,
	}

	return &configValues, nil
}

func findConfigInRelease(release *Release) *kotsv1beta1.Config {
	kotsscheme.AddToScheme(scheme.Scheme)
	for _, content := range release.Manifests {
		decode := scheme.Codecs.UniversalDeserializer().Decode
		obj, gvk, err := decode(content, nil, nil)
		if err != nil {
			continue
		}

		if gvk.Group == "kots.io" {
			if gvk.Version == "v1beta1" {
				if gvk.Kind == "Config" {
					return obj.(*kotsv1beta1.Config)
				}
			}
		}
	}

	return nil
}

func findAppInRelease(release *Release) *kotsv1beta1.Application {
	kotsscheme.AddToScheme(scheme.Scheme)
	for _, content := range release.Manifests {
		decode := scheme.Codecs.UniversalDeserializer().Decode
		obj, gvk, err := decode(content, nil, nil)
		if err != nil {
			continue
		}

		if gvk.Group == "kots.io" {
			if gvk.Version == "v1beta1" {
				if gvk.Kind == "Application" {
					return obj.(*kotsv1beta1.Application)
				}
			}
		}
	}

	// Using Ship apps for now, so let's create an app manifest on the fly
	app := &kotsv1beta1.Application{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kots.io/v1beta1",
			Kind:       "Application",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "replicated-kots-app",
		},
		Spec: kotsv1beta1.ApplicationSpec{
			Title: "Replicated Kots App",
			Icon:  "",
		},
	}
	return app
}

func releaseToFiles(release *Release) ([]UpstreamFile, error) {
	upstreamFiles := []UpstreamFile{}

	for filename, content := range release.Manifests {
		upstreamFile := UpstreamFile{
			Path:    filename,
			Content: content,
		}

		upstreamFiles = append(upstreamFiles, upstreamFile)
	}

	// Stash the user data for this search (we will readd at the end)
	userdataFiles := []UpstreamFile{}
	withoutUserdataFiles := []UpstreamFile{}
	for _, file := range upstreamFiles {
		d, _ := path.Split(file.Path)
		dirs := strings.Split(d, string(os.PathSeparator))

		if dirs[0] == "userdata" {
			userdataFiles = append(userdataFiles, file)
		} else {
			withoutUserdataFiles = append(withoutUserdataFiles, file)
		}
	}

	// remove any common prefix from all files
	if len(withoutUserdataFiles) > 0 {
		firstFileDir, _ := path.Split(withoutUserdataFiles[0].Path)
		commonPrefix := strings.Split(firstFileDir, string(os.PathSeparator))

		for _, file := range withoutUserdataFiles {
			d, _ := path.Split(file.Path)
			dirs := strings.Split(d, string(os.PathSeparator))

			commonPrefix = util.CommonSlicePrefix(commonPrefix, dirs)

		}

		cleanedUpstreamFiles := []UpstreamFile{}
		for _, file := range withoutUserdataFiles {
			d, f := path.Split(file.Path)
			d2 := strings.Split(d, string(os.PathSeparator))

			cleanedUpstreamFile := file
			d2 = d2[len(commonPrefix):]
			cleanedUpstreamFile.Path = path.Join(path.Join(d2...), f)

			cleanedUpstreamFiles = append(cleanedUpstreamFiles, cleanedUpstreamFile)
		}

		upstreamFiles = cleanedUpstreamFiles
	}

	upstreamFiles = append(upstreamFiles, userdataFiles...)

	return upstreamFiles, nil
}

// GetApplicationMetadata will return any available application yaml from
// the upstream. If there is no application.yaml, it will return
// a placeholder one
func GetApplicationMetadata(upstream *url.URL) ([]byte, error) {
	metadata, err := getApplicationMetadataFromHost("replicated.app", upstream)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get metadata from replicated.app")
	}

	if metadata == nil {
		otherMetadata, err := getApplicationMetadataFromHost("staging.replicated.app", upstream)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get metadata from staging.replicated.app")
		}

		metadata = otherMetadata
	}

	if metadata == nil {
		metadata = []byte(DefaultMetadata)
	}

	return metadata, nil
}

func getApplicationMetadataFromHost(host string, upstream *url.URL) ([]byte, error) {
	r, err := parseReplicatedURL(upstream)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse replicated upstream")
	}

	url := fmt.Sprintf("https://%s/metadata/%s", host, r.AppSlug)

	if r.Channel != nil {
		url = fmt.Sprintf("%s/%s", url, *r.Channel)
	}

	getReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to call newrequest")
	}

	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		return nil, errors.Wrap(err, "failed to execute get request")
	}

	if getResp.StatusCode == 404 {
		// no metadata is not an error
		return nil, nil
	}

	if getResp.StatusCode >= 400 {
		return nil, errors.Errorf("expected result from get request: %d", getResp.StatusCode)
	}

	defer getResp.Body.Close()

	return nil, nil
}

type FindPrivateImagesOptions struct {
	RootDir            string
	CreateAppDir       bool
	ReplicatedRegistry string
	Log                *logger.Logger
}

func (u *Upstream) FindPrivateImages(options FindPrivateImagesOptions) ([]kustomizeimage.Image, error) {
	rootDir := options.RootDir
	if options.CreateAppDir {
		rootDir = path.Join(rootDir, u.Name)
	}
	upstreamDir := path.Join(rootDir, "upstream")

	upstreamImages, err := image.GetImages(upstreamDir)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list upstream images")
	}
	fmt.Printf("++++++upstreamImages:%#v\n", upstreamImages)

	result := make([]kustomizeimage.Image, 0)
	for _, upstreamImage := range upstreamImages {
		// ParseReference requires the // prefix
		ref, err := imagedocker.ParseReference(fmt.Sprintf("//%s", upstreamImage))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse image ref:%s", upstreamImage)
		}

		remoteImage, err := ref.NewImage(context.Background(), nil)
		if err == nil {
			remoteImage.Close()
			continue
		}

		if !isUnauthorized(err) {
			fmt.Printf("+++++not unauth err:%#v\n", err)
			fmt.Printf("+++++not unauth err type:%T\n", err)
			return nil, errors.Wrapf(err, "failed to create image from ref:%s", upstreamImage)
		}

		fmt.Printf("+++++unauth for:%s\n", upstreamImage)

		image := kustomizeimage.Image{
			Name: upstreamImage,
		}
		result = append(result, image)
	}

	return result, nil
}

func isUnauthorized(err error) bool {
	if err == imagedocker.ErrUnauthorizedForCredentials {
		return true
	}

	switch err := err.(type) {
	case errcode.Errors:
		for _, e := range err {
			if isUnauthorized(e) {
				return true
			}
		}
	case errcode.Error:
		return err.Code.Descriptor().HTTPStatusCode == http.StatusUnauthorized
	}

	cause := errors.Cause(err)
	if cause == err {
		return false
	}

	return isUnauthorized(cause)
}

func parseDockerRef(image string) (imagetypes.ImageReference, error) {
	return imagedocker.ParseReference(fmt.Sprintf("//%s", image))
}