package syncer

import (
	"context"
	"fmt"
	"net/url"

	userv1 "github.com/openshift/api/user/v1"
	redhatcopv1alpha1 "github.com/redhat-cop/group-sync-operator/api/v1alpha1"
	"github.com/redhat-cop/group-sync-operator/pkg/constants"
	"github.com/redhat-cop/operator-utils/pkg/util"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	azidentity "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	az "github.com/microsoft/kiota/authentication/go/azure"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgroups "github.com/microsoftgraph/msgraph-sdk-go/groups"
	msmembers "github.com/microsoftgraph/msgraph-sdk-go/groups/item/members"
	graph "github.com/microsoftgraph/msgraph-sdk-go/models/microsoft/graph"
)

var (
	azureLogger = logf.Log.WithName("syncer_azure")
)

const (
	TenantID               = "AZURE_TENANT_ID"
	ClientID               = "AZURE_CLIENT_ID"
	ClientSecret           = "AZURE_CLIENT_SECRET"
	GraphGroupType         = "#microsoft.graph.group"
	GraphUserType          = "#microsoft.graph.user"
	GraphOdataType         = "@odata.type"
	GraphID                = "id"
	GraphDisplayName       = "displayName"
	GraphUserNameAttribute = "userPrincipalName"
)

type AzureSyncer struct {
	Name              string
	GroupSync         *redhatcopv1alpha1.GroupSync
	Provider          *redhatcopv1alpha1.AzureProvider
	Client            *msgraphsdk.GraphServiceClient
	ReconcilerBase    util.ReconcilerBase
	CredentialsSecret *corev1.Secret
	CachedGroups      map[string]*graph.Group
	CachedGroupUsers  map[string][]*graph.User
	Context           context.Context
}

func (a *AzureSyncer) Init() bool {

	a.CachedGroups = make(map[string]*graph.Group)
	a.CachedGroupUsers = make(map[string][]*graph.User)
	a.Context = context.Background()

	return false
}

func (a *AzureSyncer) Validate() error {

	validationErrors := []error{}

	credentialsSecret := &corev1.Secret{}
	err := a.ReconcilerBase.GetClient().Get(a.Context, types.NamespacedName{Name: a.Provider.CredentialsSecret.Name, Namespace: a.Provider.CredentialsSecret.Namespace}, credentialsSecret)

	if err != nil {
		validationErrors = append(validationErrors, err)
	} else {

		// Check that provided secret contains required keys
		_, tenantIDSecretFound := credentialsSecret.Data[TenantID]
		_, clientIDSecretFound := credentialsSecret.Data[ClientID]
		_, clientSecretSecretFound := credentialsSecret.Data[ClientSecret]

		if !tenantIDSecretFound || !clientIDSecretFound || !clientSecretSecretFound {
			validationErrors = append(validationErrors, fmt.Errorf("Could not find `AZURE_TENANT_ID` or `AZURE_CLIENT_ID` or `AZURE_CLIENT_SECRET` key in secret '%s' in namespace '%s", a.Provider.CredentialsSecret.Name, a.Provider.CredentialsSecret.Namespace))
		}

		a.CredentialsSecret = credentialsSecret

	}

	return utilerrors.NewAggregate(validationErrors)

}

func (a *AzureSyncer) Bind() error {

	opts := &azidentity.ClientSecretCredentialOptions{}
	opts.AuthorityHost = azidentity.AuthorityHost(getAuthorityHost(a.Provider.AuthorityHost))
	cred, err := azidentity.NewClientSecretCredential(
		string(a.CredentialsSecret.Data[TenantID]), string(a.CredentialsSecret.Data[ClientID]), string(a.CredentialsSecret.Data[ClientSecret]),
		opts)

	if err != nil {
		return err
	}

	auth, err := az.NewAzureIdentityAuthenticationProvider(cred)

	if err != nil {
		return err
	}

	adapter, err := msgraphsdk.NewGraphRequestAdapter(auth)
	if err != nil {
		return err

	}

	a.Client = msgraphsdk.NewGraphServiceClient(adapter)

	return nil

}

func (a *AzureSyncer) Sync() ([]userv1.Group, error) {

	ocpGroups := []userv1.Group{}
	aadGroups := []graph.Group{}

	if a.Provider.BaseGroups != nil && len(a.Provider.BaseGroups) > 0 {

		for _, baseGroup := range a.Provider.BaseGroups {

			filter := fmt.Sprintf("displayName eq '%s'", baseGroup)
			groupRequestParameters := &msgroups.GroupsRequestBuilderGetQueryParameters{
				Filter: &filter,
			}
			groupOptions := &msgroups.GroupsRequestBuilderGetOptions{
				Q: groupRequestParameters,
			}

			baseGroupRequest, err := a.Client.Groups().Get(groupOptions)

			if err != nil {
				azureLogger.Error(err, "Failed to get base group", "Provider", a.Name, "Base Group", baseGroup)
				return nil, err
			}

			baseGroupResult := getGroupsFromResults(baseGroupRequest)

			// Check that only 1 group was found
			if len(baseGroupResult) != 1 {
				azureLogger.Info("Failed to find a single base group to search from", "Provider", a.Name, "Base Group", baseGroup)
				continue
			}

			// Add Base Group
			aadGroups = append(aadGroups, baseGroupResult[0])

			var baseGroupMemberOptions *msmembers.MembersRequestBuilderGetOptions

			if a.Provider.Filter != "" {
				requestParameters := &msmembers.MembersRequestBuilderGetQueryParameters{
					Filter: &a.Provider.Filter,
				}
				baseGroupMemberOptions = &msmembers.MembersRequestBuilderGetOptions{
					Q: requestParameters,
				}

			}

			baseGroupMembersRequest, err := a.Client.GroupsById(*baseGroupResult[0].GetId()).Members().Get(baseGroupMemberOptions)

			if err != nil {
				azureLogger.Error(err, "Failed to get base group members", "Provider", a.Name, "Base Group", baseGroup)
				return nil, err
			}

			baseGroupMembersResult := getDirectoryObjectsFromResults(baseGroupMembersRequest)

			for _, baseGroupMember := range baseGroupMembersResult {

				baseGroupMemberODataType, _ := baseGroupMember.GetAdditionalData()[GraphOdataType].(*string)

				// Add base groups
				if GraphGroupType == *baseGroupMemberODataType {

					baseGroupDisplayNameRaw, _ := baseGroupMember.GetAdditionalData()[GraphDisplayName]
					baseGroupDisplayName := baseGroupDisplayNameRaw.(*string)
					baseGroup := graph.Group{
						DirectoryObject: baseGroupMember,
					}
					baseGroup.SetDisplayName(baseGroupDisplayName)
					aadGroups = append(aadGroups, baseGroup)
				}
			}

		}

	} else {

		var groupOptions *msgroups.GroupsRequestBuilderGetOptions

		if a.Provider.Filter != "" {
			groupRequestParameters := &msgroups.GroupsRequestBuilderGetQueryParameters{
				Filter: &a.Provider.Filter,
			}
			groupOptions = &msgroups.GroupsRequestBuilderGetOptions{
				Q: groupRequestParameters,
			}

		}

		groupRequest, err := a.Client.Groups().Get(groupOptions)

		if err != nil {
			azureLogger.Error(err, "Failed to get groups", "Provider", a.Name)
			return nil, err
		}

		groupResult := getGroupsFromResults(groupRequest)

		aadGroups = append(aadGroups, groupResult...)

	}

	authorityHost := string(getAuthorityHost(a.Provider.AuthorityHost))
	azureURL, err := url.Parse(authorityHost)
	if err != nil {
		azureLogger.Error(err, "Failed to parse Azure URL", "URL", authorityHost)
		return nil, err
	}

	for _, group := range aadGroups {

		groupName := group.GetDisplayName()

		if groupName == nil {
			azureLogger.Info(fmt.Sprintf("Warning: Skipping Group record with empty displayName"))
			continue
		}

		if !isGroupAllowed(*groupName, a.Provider.Groups) {
			continue
		}

		ocpGroup := userv1.Group{
			TypeMeta: v1.TypeMeta{
				Kind:       "Group",
				APIVersion: userv1.GroupVersion.String(),
			},
			ObjectMeta: v1.ObjectMeta{
				Name:        *groupName,
				Annotations: map[string]string{},
				Labels:      map[string]string{},
			},
			Users: []string{},
		}

		// Set Host Specific Details
		ocpGroup.GetAnnotations()[constants.SyncSourceHost] = azureURL.Host
		ocpGroup.GetAnnotations()[constants.SyncSourceUID] = *group.DirectoryObject.GetId()

		groupMembers, err := a.listGroupMembers(group.DirectoryObject.GetId())

		if err != nil {
			azureLogger.Error(err, "Failed to get Group members for Group", "Group", group.GetDisplayName(), "Provider", a.Name)
			return nil, err
		}

		for _, groupMember := range groupMembers {
			ocpGroup.Users = append(ocpGroup.Users, groupMember)
		}

		ocpGroups = append(ocpGroups, ocpGroup)

	}

	return ocpGroups, nil

}

func (a *AzureSyncer) GetProviderName() string {
	return a.Name
}

func (a *AzureSyncer) listGroupMembers(groupID *string) ([]string, error) {
	groupMembers := []string{}
	memberRequest, err := a.Client.GroupsById(*groupID).TransitiveMembers().Get(nil)

	if err != nil {
		return nil, err
	}

	members := memberRequest.GetValue()

	for _, member := range members {

		memberODataType, _ := member.GetAdditionalData()[GraphOdataType].(*string)

		if *memberODataType == GraphUserType {
			if username, found := a.getUsernameForUser(member); found {
				groupMembers = append(groupMembers, fmt.Sprintf("%v", username))
			} else {
				azureLogger.Info(fmt.Sprintf("Warning: Username for user cannot be found in Group ID '%v'", *groupID))
			}
		}

	}

	return groupMembers, nil

}

func (a *AzureSyncer) getUsernameForUser(user graph.DirectoryObjectable) (string, bool) {

	if a.Provider.UserNameAttributes == nil {
		return a.isUsernamePresent(user, GraphUserNameAttribute)
	}

	for _, usernameAttribute := range *a.Provider.UserNameAttributes {

		username, found := a.isUsernamePresent(user, usernameAttribute)

		if found {
			return username, true
		}
	}

	return "", false

}

func (a *AzureSyncer) isUsernamePresent(user graph.DirectoryObjectable, field string) (string, bool) {

	value, ok := user.GetAdditionalData()[field].(*string)
	return fmt.Sprintf("%v", *value), ok
}

func (a *AzureSyncer) GetPrune() bool {
	return a.Provider.Prune
}

func getAuthorityHost(authorityHost *string) azidentity.AuthorityHost {

	if authorityHost == nil {
		return azidentity.AzurePublicCloud

	} else {
		return azidentity.AuthorityHost(*authorityHost)
	}

}

func getGroupsFromResults(result graph.GroupCollectionResponseable) []graph.Group {
	groups := []graph.Group{}

	for _, g := range result.GetValue() {

		group := g.(*graph.Group)
		groups = append(groups, *group)

	}

	return groups
}

func getDirectoryObjectsFromResults(result graph.DirectoryObjectCollectionResponseable) []graph.DirectoryObject {
	directoryObjects := []graph.DirectoryObject{}

	for _, d := range result.GetValue() {

		directoryObject := d.(*graph.DirectoryObject)
		directoryObjects = append(directoryObjects, *directoryObject)

	}

	return directoryObjects
}