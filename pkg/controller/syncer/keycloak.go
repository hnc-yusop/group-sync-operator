package syncer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"

	"crypto/x509"

	"github.com/Nerzal/gocloak/v5"
	userv1 "github.com/openshift/api/user/v1"
	redhatcopv1alpha1 "github.com/redhat-cop/group-sync-operator/pkg/apis/redhatcop/v1alpha1"
	"github.com/redhat-cop/group-sync-operator/pkg/controller/constants"
	"github.com/redhat-cop/operator-utils/pkg/util"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	masterRealm       = "master"
	secretUsernameKey = "username"
	secretPasswordKey = "password"
	secretCaKey       = "ca.crt"
)

var (
	log    = logf.Log.WithName("syncer_keycloak")
	truthy = true
)

type KeycloakSyncer struct {
	Name               string
	GroupSync          *redhatcopv1alpha1.GroupSync
	Provider           *redhatcopv1alpha1.KeycloakProvider
	GoCloak            gocloak.GoCloak
	Token              *gocloak.JWT
	CachedGroups       map[string]*gocloak.Group
	CachedGroupMembers map[string][]*gocloak.User
	ReconcilerBase     util.ReconcilerBase
	Secret             *corev1.Secret
}

func (k *KeycloakSyncer) Init() bool {

	changed := false

	if k.Provider.LoginRealm == "" {
		k.Provider.LoginRealm = masterRealm
		changed = true
	}

	if k.Provider.Scope == "" {
		k.Provider.Scope = redhatcopv1alpha1.SubSyncScope
		changed = true
	}

	return changed

}

func (k *KeycloakSyncer) Validate() error {

	validationErrors := []error{}

	// Verify Secret Containing Username and Password Exists with Valid Keys
	secret := &corev1.Secret{}
	err := k.ReconcilerBase.GetClient().Get(context.TODO(), types.NamespacedName{Name: k.Provider.SecretName, Namespace: k.GroupSync.Namespace}, secret)

	if err != nil {
		validationErrors = append(validationErrors, err)
	}

	if _, err := url.ParseRequestURI(k.Provider.URL); err != nil {
		validationErrors = append(validationErrors, err)
	}

	// Username key validation
	if _, found := secret.Data[secretUsernameKey]; !found {
		validationErrors = append(validationErrors, fmt.Errorf("Could not find 'username' key in secret '%s' in namespace '%s", k.Provider.SecretName, k.GroupSync.Namespace))
	}

	// Password key validation
	if _, found := secret.Data[secretUsernameKey]; !found {
		validationErrors = append(validationErrors, fmt.Errorf("Could not find 'password' key in secret '%s' in namespace '%s", k.Provider.SecretName, k.GroupSync.Namespace))
	}

	k.Secret = secret

	return utilerrors.NewAggregate(validationErrors)

}

func (k *KeycloakSyncer) Bind() error {

	k.CachedGroupMembers = make(map[string][]*gocloak.User)
	k.CachedGroups = make(map[string]*gocloak.Group)

	k.GoCloak = gocloak.NewClient(k.Provider.URL)

	restyClient := k.GoCloak.RestyClient()

	if k.Provider.Insecure == true {
		restyClient.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	}

	// Add trusted certificate if provided
	if caCrt, found := k.Secret.Data[secretCaKey]; found {

		tlsConfig := &tls.Config{}
		if tlsConfig.RootCAs == nil {
			tlsConfig.RootCAs = x509.NewCertPool()
		}

		tlsConfig.RootCAs.AppendCertsFromPEM(caCrt)

		restyClient.SetTLSClientConfig(tlsConfig)
	}

	k.GoCloak.SetRestyClient(restyClient)

	token, err := k.GoCloak.LoginAdmin(string(k.Secret.Data[secretUsernameKey]), string(k.Secret.Data[secretPasswordKey]), k.Provider.LoginRealm)

	k.Token = token

	if err != nil {
		return err
	}

	log.Info("Successfully Authenticated with Keycloak Provider")

	return nil
}

func (k *KeycloakSyncer) Sync() ([]userv1.Group, error) {

	// Get Groups
	groupParams := gocloak.GetGroupsParams{Full: &truthy}
	groups, err := k.GoCloak.GetGroups(k.Token.AccessToken, k.Provider.Realm, groupParams)

	if err != nil {
		log.Error(err, "Failed to get Groups", "Provider", k.Name)
		return nil, err
	}

	for _, group := range groups {
		if _, groupFound := k.CachedGroups[*group.ID]; !groupFound {
			k.processGroupsAndMembers(group, nil, k.Provider.Scope)
		}
	}

	ocpGroups := []userv1.Group{}

	for _, cachedGroup := range k.CachedGroups {

		ocpGroup := userv1.Group{
			TypeMeta: v1.TypeMeta{
				Kind:       "Group",
				APIVersion: userv1.SchemeGroupVersion.String(),
			},
			ObjectMeta: v1.ObjectMeta{
				Name:        *cachedGroup.Name,
				Annotations: map[string]string{},
				Labels:      map[string]string{},
			},
			Users: []string{},
		}

		url, err := url.Parse(k.Provider.URL)

		if err != nil {
			return nil, err
		}

		// Set Host Specific Details
		ocpGroup.GetAnnotations()[constants.SyncSourceHost] = url.Host
		ocpGroup.GetAnnotations()[constants.SyncSourceUID] = *cachedGroup.ID

		for _, user := range k.CachedGroupMembers[*cachedGroup.ID] {
			ocpGroup.Users = append(ocpGroup.Users, *user.Username)
		}

		ocpGroups = append(ocpGroups, ocpGroup)

	}

	return ocpGroups, nil
}

func (k *KeycloakSyncer) processGroupsAndMembers(group, parentGroup *gocloak.Group, scope redhatcopv1alpha1.SyncScope) error {
	k.CachedGroups[*group.ID] = group

	groupParams := gocloak.GetGroupsParams{Full: &truthy}
	groupMembers, err := k.GoCloak.GetGroupMembers(k.Token.AccessToken, k.Provider.Realm, *group.ID, groupParams)

	if err != nil {
		return err
	}

	// Add Group Members to Primary Group
	k.CachedGroupMembers[*group.ID] = groupMembers

	if parentGroup != nil {
		k.CachedGroupMembers[*parentGroup.ID] = append(k.CachedGroupMembers[*parentGroup.ID], groupMembers...)
	}

	// Process Subgroups
	if redhatcopv1alpha1.SubSyncScope == scope {
		for _, subGroup := range group.SubGroups {
			if _, subGroupFound := k.CachedGroups[*subGroup.ID]; !subGroupFound {
				k.processGroupsAndMembers(subGroup, group, scope)
			}
		}
	}

	return nil
}

func (k *KeycloakSyncer) GetProviderName() string {
	return k.Name
}