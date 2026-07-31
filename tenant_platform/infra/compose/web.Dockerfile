# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32 AS web-build
WORKDIR /src
COPY tenant_platform/web/package.json tenant_platform/web/package-lock.json ./
RUN npm ci
COPY tenant_platform/web/index.html tenant_platform/web/vite.config.ts ./
COPY tenant_platform/web/tsconfig.json tenant_platform/web/tsconfig.app.json tenant_platform/web/tsconfig.node.json ./
COPY tenant_platform/web/public/ ./public/
COPY tenant_platform/web/src/ ./src/
RUN npm run build

FROM nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10
COPY tenant_platform/infra/compose/nginx.conf /etc/nginx/nginx.conf
COPY --from=web-build /src/dist/ /usr/share/nginx/html/
RUN chmod -R a-w /usr/share/nginx/html /etc/nginx/nginx.conf
USER 101:101
EXPOSE 8088
ENTRYPOINT ["nginx", "-g", "daemon off;"]
