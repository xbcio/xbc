// Package objectstorage provides a vendor-neutral streaming object Store and
// an XBC Plugin backed by a confined local directory or an S3-compatible API.
// Importing this package is side-effect free. Applications compose Bundle
// explicitly; executables may instead import the autoload subpackage when they
// intentionally want process-wide default composition.
//
// # Usage
//
// Configure a named entry below plugins.objectstorage, declare a typed reference
// to that instance, and read the pre-bound Store in the consuming factory:
//
//	var assetsStore = plugin.RefToInstance[objectstorage.Store](objectstorage.Key, "assets")
//
//	type assets struct {
//		store objectstorage.Store
//	}
//
//	var assetsDefinition = plugin.Define(
//		"assets-service",
//		func(ctx plugin.BuildContext) (*assets, error) {
//			return &assets{store: assetsStore.Get(ctx).Value}, nil
//		},
//		plugin.Options[*assets]{
//			Inputs: plugin.Inputs(assetsStore),
//		},
//	)
//
//	func applicationBundle() plugin.Bundle {
//		return plugin.CombineBundles(
//			objectstorage.Bundle(),
//			plugin.BundleOf(assetsDefinition),
//		)
//	}
//
//	func (a *assets) Upload(ctx context.Context, key string, src io.Reader, size int64) (objectstorage.ObjectInfo, error) {
//		return a.store.Put(ctx, key, src, objectstorage.PutOptions{Size: size})
//	}
//
//	func (a *assets) Download(ctx context.Context, key string, dst io.Writer) error {
//		object, err := a.store.Get(ctx, key)
//		if err != nil {
//			return err
//		}
//		defer object.Body.Close()
//		_, err = io.Copy(dst, object.Body)
//		return err
//	}
//
// Use UnknownSize when an upload length is not known. Put and Get remain
// streaming while enforcing MaxObjectBytes, and callers must close every Get
// body. Store implementations are concurrency-safe; XBC closes each owned Store
// once during shutdown and prevents new operations.
//
// Object keys and list prefixes are validated. The local backend confines I/O
// beneath its configured root, while the S3 backend supports TLS and bounded
// network timeouts. Keep S3 credentials in secret-backed configuration and do
// not enable SkipVerify outside isolated development environments.
package objectstorage
