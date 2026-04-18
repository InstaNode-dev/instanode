alerts:
- rule: DEPLOYMENT_FAILED
- rule: DOMAIN_FAILED
databases:

- cluster_name: instant-redis-cluster
  engine: REDIS
  name: instant-redis-cluster
  production: true
  version: "8"
domains:
- domain: api.instanode.dev
  type: PRIMARY
enhanced_threat_control_enabled: false
features:
- buildpack-stack=ubuntu-22
ingress:
  rules:
  - component:
      name: instant-lite-api
    match:
      authority: {}
      path:
        prefix: /
name: insta-node-lite
region: atl
services:
- dockerfile_path: /Dockerfile
  envs:
  - key: APP_URL
    scope: RUN_TIME
    value: https://instanode.dev
  - key: DATABASE_URL
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_PLATFORM_DB_URL
  - key: NEWRELIC_LICENSE_KEY
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_NEWRELIC_LICENSE
  - key: REDIS_URL
    scope: RUN_TIME
    value: ${instant-redis-cluster.REDIS_URL}
  - key: GITHUB_CLIENT_ID
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_GITHUB_CLIENT_ID
  - key: GITHUB_CLIENT_SECRET
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_GITHUB_CLIENT_SECRET
  - key: RAZORPAY_KEY_ID
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_RAZORPAY_KEY_ID
  - key: RAZORPAY_KEY_SECRET
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_RAZORPAY_KEY_SECRET
  - key: RAZORPAY_WEBHOOK_SECRET
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_RAZORPAY_WEBHOOK_SECRET
  - key: JWT_SECRET
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_JWT_SECRET
  - key: CUSTOMER_DATABASE_URL
    scope: RUN_TIME
    type: SECRET
    value: REPLACE_ME_CUSTOMER_DB_URL
  - key: POSTGRES_PUBLIC_HOST
    scope: RUN_TIME
    value: pg.instanode.dev
  - key: POSTGRES_PUBLIC_PORT
    scope: RUN_TIME
    value: "5432"
  - key: POSTGRES_REQUIRE_TLS
    scope: RUN_TIME
    value: "true"
  github:
    branch: master
    deploy_on_push: true
    repo: InstaNode-dev/instant-lite-api
  http_port: 8080
  instance_count: 2
  instance_size_slug: apps-s-1vcpu-1gb
  name: instant-lite-api
  source_dir: /
