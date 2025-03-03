FROM registry.ci.openshift.org/openshift/release:golang-1.22 AS builder
WORKDIR /go/src/github.com/openshift-splat-team/machine-ipam-controller
COPY . .
ENV GO_PACKAGE github.com/openshift-splat-team/machine-ipam-controller
RUN NO_DOCKER=1 make build

# FROM registry.ci.openshift.org/openshift/origin-v4.0:base
# FROM registry.ci.openshift.org/ocp/4.13:base
FROM registry.ci.openshift.org/openshift/origin-v4.0:base
COPY --from=builder /go/src/github.com/openshift-splat-team/machine-ipam-controller/install manifests
COPY --from=builder /go/src/github.com/openshift-splat-team/machine-ipam-controller/bin/mapi-static-ip-controller /usr/bin/mapi-static-ip-controller
ENTRYPOINT ["/usr/bin/mapi-static-ip-controller"]
LABEL io.openshift.release.operator=true

