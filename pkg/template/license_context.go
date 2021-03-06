package template

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"text/template"

	kotsv1beta1 "github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
)

type LicenseCtx struct {
	License *kotsv1beta1.License
}

// FuncMap represents the available functions in the LicenseCtx.
func (ctx LicenseCtx) FuncMap() template.FuncMap {
	return template.FuncMap{
		"LicenseFieldValue": ctx.licenseFieldValue,
		"LicenseDockerCfg":  ctx.licenseDockercfg,
	}
}

func (ctx LicenseCtx) licenseFieldValue(name string) string {
	// return "" for a nil license - it's better than an error, which makes the template engine return "" for the full string
	if ctx.License == nil {
		return ""
	}

	entitlement, ok := ctx.License.Spec.Entitlements[name]
	if ok {
		return fmt.Sprintf("%v", entitlement.Value.Value())
	}
	return ""
}

func (ctx LicenseCtx) licenseDockercfg() string {
	// return "" for a nil license - it's better than an error, which makes the template engine return "" for the full string
	if ctx.License == nil {
		return ""
	}

	auth := fmt.Sprintf("%s:%s", ctx.License.Spec.LicenseID, ctx.License.Spec.LicenseID)
	encodedAuth := base64.StdEncoding.EncodeToString([]byte(auth))

	dockercfg := map[string]interface{}{
		"auths": map[string]interface{}{
			"proxy.replicated.com": map[string]string{
				"auth": encodedAuth,
			},
			"registry.replicated.com": map[string]string{
				"auth": encodedAuth,
			},
		},
	}

	b, err := json.Marshal(dockercfg)
	if err != nil {
		return ""
	}

	return base64.StdEncoding.EncodeToString(b)
}
