package cliutil

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	grpcCodes "google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/client/errcat"
	"github.com/telepresenceio/telepresence/v2/pkg/client/userd/auth/authdata"
)

// EnsureLoggedIn ensures that the user is logged in to Ambassador Cloud.  An error is returned if
// login fails.  The result code will indicate if this is a new login or if it resued an existing
// login.  If the `apikey` argument is empty an interactive login is performed; if it is non-empty
// the key is used instead of performing an interactive login.
func EnsureLoggedIn(ctx context.Context, apikey string) (connector.LoginResult_Code, error) {
	var code connector.LoginResult_Code
	telProBinary, err := GetTelepresencePro(ctx)
	if err != nil {
		return connector.LoginResult_UNSPECIFIED, err
	}
	err = WithConnector(ctx, telProBinary, func(ctx context.Context, connectorClient connector.ConnectorClient) error {
		var err error
		code, err = ClientEnsureLoggedIn(ctx, apikey, connectorClient)
		return err
	})
	return code, err
}

// ClientEnsureLoggedIn is like EnsureLoggedIn but uses an already acquired ConnectorClient.
func ClientEnsureLoggedIn(ctx context.Context, apikey string, connectorClient connector.ConnectorClient) (connector.LoginResult_Code, error) {
	resp, err := connectorClient.Login(ctx, &connector.LoginRequest{
		ApiKey: apikey,
	})
	if err != nil {
		if grpcStatus.Code(err) == grpcCodes.PermissionDenied {
			err = errcat.User.New(grpcStatus.Convert(err).Message())
		}
		return connector.LoginResult_UNSPECIFIED, err
	}
	return resp.GetCode(), nil
}

// Logout logs out of Ambassador Cloud.  Returns an error if not logged in.
func Logout(ctx context.Context) error {
	/*
		telProBinary, err := GetTelepresencePro(ctx)
		if err != nil {
			return err
		}
	*/
	err := WithConnector(ctx, "", func(ctx context.Context, connectorClient connector.ConnectorClient) error {
		_, err := connectorClient.Logout(ctx, &empty.Empty{})
		return err
	})
	if grpcStatus.Code(err) == grpcCodes.NotFound {
		err = errcat.User.New(grpcStatus.Convert(err).Message())
	}
	if err != nil {
		return err
	}
	return nil
}

// EnsureLoggedOut ensures that the user is logged out of Ambassador Cloud.  Returns nil if not
// logged in.
func EnsureLoggedOut(ctx context.Context) error {
	/*
		telProBinary, err := GetTelepresencePro(ctx)
		if err != nil {
			return err
		}
	*/
	err := WithConnector(ctx, "", func(ctx context.Context, connectorClient connector.ConnectorClient) error {
		_, err := connectorClient.Logout(ctx, &empty.Empty{})
		return err
	})
	if grpcStatus.Code(err) == grpcCodes.NotFound {
		err = nil
	}
	if err != nil {
		return err
	}
	return nil
}

// HasLoggedIn returns true if either the user has an active login session or an expired login
// session, and returns false if either the user has never logged in or has explicitly logged out.
func HasLoggedIn(ctx context.Context) bool {
	_, err := authdata.LoadUserInfoFromUserCache(ctx)
	return err == nil
}

func GetCloudUserInfo(ctx context.Context, autoLogin bool, refresh bool) (*connector.UserInfo, error) {
	var userInfo *connector.UserInfo
	telProBinary, err := GetTelepresencePro(ctx)
	if err != nil {
		return userInfo, err
	}
	err = WithConnector(ctx, telProBinary, func(ctx context.Context, connectorClient connector.ConnectorClient) error {
		var err error
		userInfo, err = connectorClient.GetCloudUserInfo(ctx, &connector.UserInfoRequest{
			AutoLogin: autoLogin,
			Refresh:   refresh,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return userInfo, nil
}

func GetCloudAPIKey(ctx context.Context, description string, autoLogin bool) (string, error) {
	var keyData *connector.KeyData
	telProBinary, err := GetTelepresencePro(ctx)
	if err != nil {
		return "", err
	}
	err = WithConnector(ctx, telProBinary, func(ctx context.Context, connectorClient connector.ConnectorClient) error {
		var err error
		keyData, err = connectorClient.GetCloudAPIKey(ctx, &connector.KeyRequest{
			AutoLogin:   autoLogin,
			Description: description,
		})
		return err
	})
	if err != nil {
		return "", err
	}
	return keyData.GetApiKey(), nil
}

// GetCloudLicense communicates with system a to get the jwt version of the
// license, puts it in a kubernetes secret, and then writes that secret to the
// output file for the user to apply to their cluster
func GetCloudLicense(ctx context.Context, outputFile, id string) (string, string, error) {
	var licenseData *connector.LicenseData
	telProBinary, err := GetTelepresencePro(ctx)
	if err != nil {
		return "", "", err
	}
	err = WithConnector(ctx, telProBinary, func(ctx context.Context, connectorClient connector.ConnectorClient) error {
		var err error
		licenseData, err = connectorClient.GetCloudLicense(ctx, &connector.LicenseRequest{
			Id: id,
		})
		return err
	})
	if err != nil {
		return "", "", err
	}
	return licenseData.GetLicense(), licenseData.GetHostDomain(), nil
}

func GetTelepresencePro(ctx context.Context) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", errcat.Unknown.Newf("Unable to get path for executable: %s", err)
	}
	telProLocation := fmt.Sprintf("%s/telepresence-pro", filepath.Dir(executable))
	if _, err := os.Stat(telProLocation); os.IsNotExist(err) {
		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("Telepresence Pro is required to use login features, can Telepresence install it? (y/n)")
		reply, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}

		reply = strings.TrimSpace(reply)
		if reply == "n" {
			return "", errcat.User.New("Telepresence Pro must be installed to login\n")
		}
		// TODO: replace the hardcoded 0.0.1 with this once publishing is working
		clientVersion := strings.Trim(client.Version(), "v")
		systemAHost := client.GetConfig(ctx).Cloud.SystemaHost
		installString := fmt.Sprintf("https://%s/download/tel-pro/%s/%s/0.0.1/telepresence-pro", systemAHost, runtime.GOOS, runtime.GOARCH)
		fmt.Printf("installing %s version %s to %s\n", installString, clientVersion, telProLocation)

		resp, err := http.Get(installString)
		if err != nil {
			return "", errcat.User.Newf("unable to install Telepresence Pro: %s", err)
		}
		defer resp.Body.Close()

		out, err := os.Create(telProLocation)
		if err != nil {
			return "", errcat.User.Newf("unable to create file %s for Telepresence Pro: %s", telProLocation, err)
		}
		defer out.Close()

		_, err = io.Copy(out, resp.Body)
		if err != nil {
			return "", errcat.User.Newf("unable to copy Telepresence Pro to %s: %s", telProLocation, err)
		}

		err = os.Chmod(telProLocation, 0755)
		if err != nil {
			return "", errcat.User.Newf("unable to set permissions of Telepresence Pro to 755: %s", err)
		}

	}
	return telProLocation, nil
}
