package protocol

import (
	"net/http"
	"os"
	"time"

	"github.com/go-openapi/runtime/middleware"
	"github.com/go-openapi/swag"
	"github.com/powerman/structlog"

	awsSdk "github.com/aws/aws-sdk-go/aws"

	"git.arilot.com/kuberstack/kuberstack-installer/db"
	"git.arilot.com/kuberstack/kuberstack-installer/kops"
	"git.arilot.com/kuberstack/kuberstack-installer/predefined"
	"git.arilot.com/kuberstack/kuberstack-installer/protocol/gen/models"
	"git.arilot.com/kuberstack/kuberstack-installer/protocol/gen/restapi/operations"
	"git.arilot.com/kuberstack/kuberstack-installer/protocol/gen/restapi/operations/installer"
	"git.arilot.com/kuberstack/kuberstack-installer/protocol/responder"
	"git.arilot.com/kuberstack/kuberstack-installer/savedstate"
	"git.arilot.com/kuberstack/kuberstack-installer/steps/auth"
	"git.arilot.com/kuberstack/kuberstack-installer/steps/aws"
	"git.arilot.com/kuberstack/kuberstack-installer/steps/cluster"
	"git.arilot.com/kuberstack/kuberstack-installer/steps/install"
	"git.arilot.com/kuberstack/kuberstack-installer/steps/nodes"
	"git.arilot.com/kuberstack/kuberstack-installer/steps/software"
)

var dbConfig struct {
	Driver     string        `long:"dbDriver" description:"database driver to use (bolt)" default:"bolt" env:"DBDRIVER"`
	URI        string        `long:"dbURI" description:"database URI to connect" default:"./kuberstack-installer.db" env:"DBURI"`
	AuthExpire time.Duration `long:"authExpire" description:"Time to get incomplete session expired" default:"8760h" env:"DBAUTHEXPIRE"`
}

var (
	kopsConfig kops.Config

// 	kubectlConfig kubectl.Config
)

// ConfigureFlags called by the autogenerated code just before parsing the flags to add/configure these flags.
func ConfigureFlags(api *operations.KuberstackInstallerAPI) {
	// api.CommandLineOptionsGroups = []swag.CommandLineOptionsGroup{ ... }
	api.CommandLineOptionsGroups = append(
		api.CommandLineOptionsGroups,
		swag.CommandLineOptionsGroup{
			ShortDescription: "DB options",
			LongDescription:  "Local storage parameters",
			Options:          &dbConfig,
		},
		swag.CommandLineOptionsGroup{
			ShortDescription: "Kops options",
			LongDescription:  "Jus a way to run embedded kops binary",
			Options:          &kopsConfig,
		},
	)
}

// ConfigureAPI called by the autogenerated code to set up the real callbacks for the API.
func ConfigureAPI(api *operations.KuberstackInstallerAPI) {
	cmdItself := os.Args[0]
	logger := structlog.New()

	if code, itIs := kops.CheckKopsRun(kopsConfig, cmdItself, logger); itIs {
		os.Exit(code)
	}

	logger.Info("Started", "AuthExpire", dbConfig.AuthExpire)

	conn, err := db.Open(dbConfig.Driver, dbConfig.URI, dbConfig.AuthExpire)
	if err != nil {
		panic(err)
	}

	api.Logger = logger.New().AddCallDepth(2).Printf

	// ToDo: find more convenient and obvious place to run this goroutine
	go db.CleanupLoop(api.Logger, conn, dbConfig.AuthExpire/2)

	apiShutdown := api.ServerShutdown
	api.ServerShutdown = func() {
		serverShutdown(conn, api, apiShutdown)
	}

	api.APIKeyHeaderAuth = func(token string) (interface{}, error) {
		return tokenAuth(conn, token)
	}

	api.InstallerGetSessionIDHandler = installer.GetSessionIDHandlerFunc(
		func(
			params installer.GetSessionIDParams,
		) middleware.Responder {
			id, err := auth.GetSessionID(conn)
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.OK(
				&models.GetSessionIDOKBody{
					Status: true,
					Token:  id,
				},
			)
		},
	)

	api.InstallerGetRegionsHandler = installer.GetRegionsHandlerFunc(
		func(
			params installer.GetRegionsParams,
			principal interface{},
		) middleware.Responder {
			return responder.OK(
				&models.GetRegionsOKBody{
					Status:  true,
					Regions: aws.GetRegions(),
				},
			)
		},
	)

	api.InstallerPutCredentialsHandler = installer.PutCredentialsHandlerFunc(
		func(
			params installer.PutCredentialsParams,
			principal interface{},
		) middleware.Responder {
			if params.Body == nil {
				return responder.NotOK("AWS credentials are not provided")
			}
			err := aws.SaveCredentials(
				conn,
				awsSdk.StringValue(params.Body.AccessKey),
				awsSdk.StringValue(params.Body.SecretKey),
				awsSdk.StringValue(params.Body.Region),
				awsSdk.StringValue(params.Body.SSHPubKey),
				*(principal.(*savedstate.Principal)),
			)
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.SimpleOK()
		},
	)

	api.InstallerGetClusterTypesHandler = installer.GetClusterTypesHandlerFunc(
		func(
			params installer.GetClusterTypesParams,
			principal interface{},
		) middleware.Responder {
			return responder.OK(
				&models.GetClusterTypesOKBody{
					Status: true,
					Types:  cluster.GetTypes(),
				},
			)
		},
	)

	api.InstallerGetDomainsHandler = installer.GetDomainsHandlerFunc(
		func(
			params installer.GetDomainsParams,
			principal interface{},
		) middleware.Responder {
			domains, err := cluster.GetDomains(*(principal.(*savedstate.Principal)))
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.OK(
				&models.GetDomainsOKBody{
					Status:  true,
					Domains: domains,
				},
			)
		},
	)

	api.InstallerSaveClusterHandler = installer.SaveClusterHandlerFunc(
		func(
			params installer.SaveClusterParams,
			principal interface{},
		) middleware.Responder {
			err := cluster.Save(
				conn,
				awsSdk.StringValue(params.Body.Domain),
				awsSdk.StringValue(params.Body.Name),
				awsSdk.Int64Value(params.Body.Type),
				*(principal.(*savedstate.Principal)),
			)
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.SimpleOK()
		},
	)

	api.InstallerCheckClusterValidityHandler = installer.CheckClusterValidityHandlerFunc(
		func(
			params installer.CheckClusterValidityParams,
			principal interface{},
		) middleware.Responder {
			err := cluster.CheckDomain(
				conn,
				awsSdk.StringValue(params.Body.Domain),
				awsSdk.StringValue(params.Body.Name),
				*(principal.(*savedstate.Principal)),
			)
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.SimpleOK()
		},
	)

	api.InstallerCheckDNSInSyncHandler = installer.CheckDNSInSyncHandlerFunc(
		func(
			params installer.CheckDNSInSyncParams,
			principal interface{},
		) middleware.Responder {
			inSync, err := cluster.IsDNSInSync(
				*(principal.(*savedstate.Principal)),
			)
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.OK(
				&models.CheckDNSInSyncOKBody{
					Status: true,
					Insync: inSync,
				},
			)
		})

	api.InstallerGetNodesTypesHandler = installer.GetNodesTypesHandlerFunc(
		func(
			params installer.GetNodesTypesParams,
			principal interface{},
		) middleware.Responder {
			return responder.OK(
				&models.GetNodesTypesOKBody{
					Status:     true,
					NodesTypes: nodes.GetNodeTypes(),
				},
			)
		},
	)

	api.InstallerGetStorageTypesHandler = installer.GetStorageTypesHandlerFunc(
		func(
			params installer.GetStorageTypesParams,
			principal interface{},
		) middleware.Responder {
			return responder.OK(
				&models.GetStorageTypesOKBody{
					Status:       true,
					StorageTypes: nodes.GetNodeTypes(),
				},
			)
		},
	)

	api.InstallerGetZonesListHandler = installer.GetZonesListHandlerFunc(
		func(
			params installer.GetZonesListParams,
			principal interface{},
		) middleware.Responder {
			zones, err := nodes.GetZones(params.Region, *(principal.(*savedstate.Principal)))
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.OK(
				&models.GetZonesListOKBody{
					Status: true,
					Zones:  zones,
				},
			)
		},
	)

	api.InstallerSaveNodesHandler = installer.SaveNodesHandlerFunc(
		func(
			params installer.SaveNodesParams,
			principal interface{},
		) middleware.Responder {
			err := nodes.Save(
				conn,
				*params.Body.Master,
				*params.Body.Nodes,
				*(principal.(*savedstate.Principal)),
			)
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.SimpleOK()
		},
	)

	api.InstallerGetSoftwareProductsHandler = installer.GetSoftwareProductsHandlerFunc(
		func(
			params installer.GetSoftwareProductsParams,
			principal interface{},
		) middleware.Responder {
			// if params.Body == nil {
			// 	params.Body = &installer.GetSoftwareProductsPara
			// 	return responder.NotOK("Filters are not provided")
			// }
			return responder.OK(
				&models.GetSoftwareProductsOKBody{
					Status: true,
					Products: software.GetProducts(
						awsSdk.StringValue(params.Search),
						params.Tags,
					),
				},
			)
		},
	)

	api.InstallerGetSoftwareTagsHandler = installer.GetSoftwareTagsHandlerFunc(
		func(
			params installer.GetSoftwareTagsParams,
			principal interface{},
		) middleware.Responder {
			return responder.OK(
				&models.GetSoftwareTagsOKBody{
					Status: true,
					Tags:   software.GetTags(),
				},
			)
		},
	)

	api.InstallerSaveSoftwareHandler = installer.SaveSoftwareHandlerFunc(
		func(
			params installer.SaveSoftwareParams,
			principal interface{},
		) middleware.Responder {
			err := software.Save(
				conn,
				params.Body.Products,
				*(principal.(*savedstate.Principal)),
			)
			if err != nil {
				return responder.NotOK(err.Error())
			}
			return responder.SimpleOK()
		},
	)

	api.InstallerInstallTheClusterHandler = installer.InstallTheClusterHandlerFunc(
		func(
			params installer.InstallTheClusterParams,
			principal interface{},
		) middleware.Responder {
			principalItself := *(principal.(*savedstate.Principal))

			err := install.Install(
				conn,
				principalItself,
				cmdItself,
				kopsConfig.TmpDir,
				kopsConfig.Timeout,
				logger,
			)
			if err != nil {
				return responder.NotOK(err.Error())
			}

			return responder.SimpleOK()
		},
	)

	api.InstallerGetInstallStatusHandler = installer.GetInstallStatusHandlerFunc(
		func(
			params installer.GetInstallStatusParams,
			principal interface{},
		) middleware.Responder {
			length, current := install.GetStatus(
				*(principal.(*savedstate.Principal)),
				cmdItself,
				kopsConfig.TmpDir,
				kopsConfig.Timeout,
				logger,
			)

			return responder.OK(
				&models.GetInstallStatusOKBody{
					Status:  true,
					Length:  int64(length),
					Current: int64(current),
				},
			)
		},
	)

	api.InstallerGetClusterConfHandler = installer.GetClusterConfHandlerFunc(
		func(
			params installer.GetClusterConfParams,
			principal interface{},
		) middleware.Responder {
			principalItself := *(principal.(*savedstate.Principal))

			resp := models.InstallOKBody{
				Status:   true,
				Domain:   principalItself.Sess.Domain,
				Name:     principalItself.Sess.Name,
				Software: make(models.InstallOKBodySoftware, len(principalItself.Sess.Products)),
				Bucketid: principalItself.Sess.Bucket,
				Master: &models.NodesProperties{
					Instances: principalItself.Sess.Master.Quantity,
					Zones:     principalItself.Sess.Master.Zones,
				},
				Node: &models.NodesProperties{
					Instances: principalItself.Sess.Nodes.Quantity,
					Zones:     principalItself.Sess.Nodes.Zones,
				},
			}

			for ri, productID := range principalItself.Sess.Products {
				resp.Software[ri] = &models.InstallProductsItems{
					ID:   productID,
					Name: predefined.GetProductNameByID(productID),
				}
			}

			return responder.OK(resp)
		},
	)

	api.InstallerGetK8sConfigHandler = installer.GetK8sConfigHandlerFunc(
		func(
			params installer.GetK8sConfigParams,
			principal interface{},
		) middleware.Responder {
			kubecfg := install.GetKubecfg(*(principal.(*savedstate.Principal)))

			return responder.NewFileResponder("kube.config", "text/plain", kubecfg)
		},
	)

	api.InstallerInstallVanishHandler = installer.InstallVanishHandlerFunc(
		func(
			params installer.InstallVanishParams,
			principal interface{},
		) middleware.Responder {
			principalItself := *(principal.(*savedstate.Principal))

			err := install.Vanish(
				conn,
				principalItself,
				cmdItself,
				kopsConfig.TmpDir,
				kopsConfig.Timeout,
				logger,
			)
			if err != nil {
				return responder.NotOK(err.Error())
			}

			return responder.SimpleOK()
		},
	)

}

// GlobalMiddleware is a log-enabled HTTP Handler wrapper
type GlobalMiddleware struct {
	Handler http.Handler
}

// ServeHTTP runs an undrlaing Handler.
// In the case of underlaing Handler paniced
// the panic will be caught and logged,
// Internal Server Error willr be returned to the client.
func (m GlobalMiddleware) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	err := func() (err error) {
		defer structlog.New().AddCallDepth(2).Recover(&err)

		if origin := r.Header.Get("Origin"); origin != "" {
			rw.Header().Set("Access-Control-Allow-Origin", origin)
			rw.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
			rw.Header().Set("Access-Control-Allow-Headers",
				"Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-Api-Key")
		}
		// Stop here if its Preflighted OPTIONS request
		if r.Method == "OPTIONS" {
			return
		}

		m.Handler.ServeHTTP(rw, r)
		return
	}()

	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
	}
}

func serverShutdown(
	conn db.Connect,
	api *operations.KuberstackInstallerAPI,
	parentShutdown func(),
) {
	err := conn.Close()
	if err != nil {
		api.Logger("Error closing database %q: %v", conn.String(), err)
	}
	if parentShutdown != nil {
		parentShutdown()
	}
}

func tokenAuth(conn db.Connect, token string) (interface{}, error) {
	if token == "" {
		return nil, nil
	}

	principal, err := auth.GetSession(conn, token)
	if err != nil {
		return nil, err
	}

	if principal == nil {
		return nil, nil
	}

	return principal, nil
}