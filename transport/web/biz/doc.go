// Package biz provides an optional HTTP business-response contract for
// XBC Web. It adopts GFA's useful explicit success-envelope and business-error
// ideas while preserving semantic HTTP statuses and RFC 9457 failures.
//
// # Usage
//
// Enable the biz.onerror middleware by composing its Bundle explicitly:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		biz "github.com/xbcio/xbc/transport/web/biz"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			biz.Bundle(),
//		))
//	}
//
// Handlers opt into the success envelope explicitly; the plugin never wraps
// arbitrary JSON, streams, files, or third-party handlers behind their back:
//
//	r.POST("/orders", web.Handle(func(c *gin.Context) error {
//		order, err := service.Create(c.Request.Context())
//		if err != nil {
//			return err
//		}
//		return biz.Created(c, order)
//	}))
//
// A client-safe business rule failure can be returned from the HTTP application
// boundary and will become application/problem+json:
//
//	return biz.NewStatusError(
//		http.StatusConflict,
//		"ORDER.ALREADY_PAID",
//		"The order has already been paid.",
//	)
//
// NewError defaults to HTTP 422. Invalid statuses or codes fail closed as a
// fixed 500. Error causes and unknown errors are never copied to the response.
// The middleware is built with web.OnError and composes with Web's outer safe
// boundary; panic recovery remains a separate responsibility. Domain packages
// should not import this package: keep protocol-neutral errors there and map
// them with web.ErrorMapper in an application plugin.
package biz
