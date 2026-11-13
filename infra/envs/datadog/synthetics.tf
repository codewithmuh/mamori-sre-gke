# One multistep API test per critical user journey (PRD CUJ-1 to CUJ-4), run against the public
# hostname from Datadog's Tokyo location on AWS: outside Google Cloud, through DNS, TLS, the load
# balancer and Cloud Armor, the way a shopper arrives. The steps match the in-cluster prober
# (tools/prober/journey.go) so the two signals can be compared run for run.
#
# Resource names are fixed by the build contract: tools/slo-gen's Datadog SLOs refer to
# datadog_synthetics_test.cuj_<id>.monitor_id.
#
# Page markers come from Online Boutique v0.10.7 (src/frontend/templates): home.html prints
# "Hot Products", product.html an "Add To Cart" button, cart.html a "Place Order" button only when
# the cart has items, order.html "Your order is complete!". Routes and form fields come from
# src/frontend/main.go, handlers.go and validator/validator.go.

locals {
  frontend_url = "https://${var.frontend_hostname}"

  # The banner deploy/online-boutique/kustomization.yaml sets (FRONTEND_MESSAGE). Checked on the
  # home page because it is a safety control for a public shop with a card form.
  banner_text = "Demo shop, no real payments"

  browse_product = { id = "OLJCESPC7Z", name = "Sunglasses" }
  cart_product   = { id = "66VCHSJNUP", name = "Tank Top" }

  # Every run is a new shopper. The frontend takes any shop_session-id cookie value as the session
  # (src/frontend/middleware.go), so the test makes its own from Datadog's {{ uuid }} built-in
  # instead of parsing Set-Cookie between steps. The prefix marks synthetic carts in redis-cart.
  session_cookie = "shop_session-id=dd-synthetics-{{ SESSION_ID }}"

  # The checkout form as cart.html pre-fills it: the demo's fake Visa card. The payment service
  # rejects expired cards, so the year is fixed in the future (good until the end of 2029).
  checkout_form = join("&", [for k, v in {
    email                        = "someone@example.com"
    street_address               = "1600 Amphitheatre Parkway"
    zip_code                     = "94043"
    city                         = "Mountain View"
    state                        = "CA"
    country                      = "United States"
    credit_card_number           = "4432801561520454"
    credit_card_expiration_month = "1"
    credit_card_expiration_year  = "2030"
    credit_card_cvv              = "672"
  } : "${k}=${urlencode(v)}"])

  slow_step = { type = "responseTime", operator = "lessThan", property = null, target = tostring(var.synthetics_max_step_ms) }

  # Steps share one shape so the dynamic blocks below can render any of them. A step with
  # critical = false is clean-up: it may fail without failing the run.
  step_home = {
    name     = "home page"
    method   = "GET"
    path     = "/"
    form     = null
    critical = true
    assertions = [
      { type = "statusCode", operator = "is", property = null, target = "200" },
      { type = "body", operator = "contains", property = null, target = "Hot Products" },
      { type = "body", operator = "contains", property = null, target = local.banner_text },
      local.slow_step,
    ]
  }

  step_product = {
    name     = "product page"
    method   = "GET"
    path     = "/product/${local.browse_product.id}"
    form     = null
    critical = true
    assertions = [
      { type = "statusCode", operator = "is", property = null, target = "200" },
      { type = "body", operator = "contains", property = null, target = local.browse_product.name },
      { type = "body", operator = "contains", property = null, target = "Add To Cart" },
      local.slow_step,
    ]
  }

  # The frontend answers 302 to /cart only after cartservice stored the item (addToCartHandler).
  # Not followed, like the prober, so add-to-cart doesn't also test the cart page.
  step_add_to_cart = {
    name     = "add to cart"
    method   = "POST"
    path     = "/cart"
    form     = "product_id=${local.cart_product.id}&quantity=1"
    critical = true
    assertions = [
      { type = "statusCode", operator = "is", property = null, target = "302" },
      { type = "header", operator = "is", property = "location", target = "/cart" },
      local.slow_step,
    ]
  }

  step_view_cart = {
    name     = "cart page"
    method   = "GET"
    path     = "/cart"
    form     = null
    critical = true
    assertions = [
      { type = "statusCode", operator = "is", property = null, target = "200" },
      { type = "body", operator = "contains", property = null, target = local.cart_product.name },
      { type = "body", operator = "contains", property = null, target = "Place Order" },
      local.slow_step,
    ]
  }

  step_place_order = {
    name     = "place order"
    method   = "POST"
    path     = "/cart/checkout"
    form     = local.checkout_form
    critical = true
    assertions = [
      { type = "statusCode", operator = "is", property = null, target = "200" },
      { type = "body", operator = "contains", property = null, target = "Your order is complete!" },
      local.slow_step,
    ]
  }

  # Empties the run's cart so synthetic carts don't pile up in redis-cart (the order does that
  # for checkout). Its failure says nothing about the journey.
  step_empty_cart = {
    name     = "clean-up: empty cart"
    method   = "POST"
    path     = "/cart/empty"
    form     = ""
    critical = false
    assertions = [
      { type = "statusCode", operator = "is", property = null, target = "302" },
    ]
  }

  cuj_steps = {
    browse        = [local.step_home, local.step_product]
    "add-to-cart" = [local.step_home, local.step_add_to_cart, local.step_empty_cart]
    "view-cart"   = [local.step_home, local.step_add_to_cart, local.step_view_cart, local.step_empty_cart]
    checkout      = [local.step_home, local.step_add_to_cart, local.step_view_cart, local.step_place_order]
  }

  synthetics_monitor_name = { for id in keys(local.cuj_steps) : id => "[mamori] CUJ ${id} failing from outside Google Cloud (Datadog synthetic)" }

  synthetics_message = { for id in keys(local.cuj_steps) : id => trimspace(<<-EOT
    The ${id} journey failed from Datadog's location ${join(", ", var.synthetics_locations)} against ${local.frontend_url}.
    This is the outside view: it also covers DNS, the certificate, the load balancer and Cloud Armor.
    Before acting, compare with the in-cluster prober (mamori_cuj_probe_total{cuj="${id}"}) and the
    Cloud Monitoring SLO for ${id}: prober green and this red points at the edge, both red at the shop.
    Count this alert in the window log (docs/datadog-window.md): actionable or not, and why.
    Runbook: ${local.runbook_base_url}/cuj-${id}.md
    ${local.notify}
    EOT
  ) }
}

resource "datadog_synthetics_test" "cuj_browse" {
  name      = "mamori CUJ browse"
  type      = "api"
  subtype   = "multi"
  status    = var.synthetics_status
  locations = var.synthetics_locations
  tags      = concat(local.datadog_tags, ["cuj:browse"])
  message   = local.synthetics_message["browse"]

  config_variable {
    type    = "text"
    name    = "SESSION_ID"
    pattern = "{{ uuid }}"
  }

  dynamic "api_step" {
    for_each = local.cuj_steps["browse"]

    content {
      name          = api_step.value.name
      subtype       = "http"
      allow_failure = !api_step.value.critical
      is_critical   = api_step.value.critical ? null : false

      request_definition {
        method           = api_step.value.method
        url              = "${local.frontend_url}${api_step.value.path}"
        body             = api_step.value.form
        body_type        = api_step.value.form == null ? null : "application/x-www-form-urlencoded"
        follow_redirects = false
        timeout          = 10
      }

      request_headers = merge(
        { Cookie = local.session_cookie },
        api_step.value.form == null ? {} : { "Content-Type" = "application/x-www-form-urlencoded" },
      )

      dynamic "assertion" {
        for_each = api_step.value.assertions

        content {
          type     = assertion.value.type
          operator = assertion.value.operator
          property = assertion.value.property
          target   = assertion.value.target
        }
      }
    }
  }

  options_list {
    tick_every           = var.synthetics_tick_every
    min_failure_duration = var.synthetics_min_failure_duration
    min_location_failed  = 1
    monitor_name         = local.synthetics_monitor_name["browse"]
    monitor_priority     = 3

    retry {
      count    = var.synthetics_retry_count
      interval = 2000
    }
  }
}

resource "datadog_synthetics_test" "cuj_add_to_cart" {
  name      = "mamori CUJ add-to-cart"
  type      = "api"
  subtype   = "multi"
  status    = var.synthetics_status
  locations = var.synthetics_locations
  tags      = concat(local.datadog_tags, ["cuj:add-to-cart"])
  message   = local.synthetics_message["add-to-cart"]

  config_variable {
    type    = "text"
    name    = "SESSION_ID"
    pattern = "{{ uuid }}"
  }

  dynamic "api_step" {
    for_each = local.cuj_steps["add-to-cart"]

    content {
      name          = api_step.value.name
      subtype       = "http"
      allow_failure = !api_step.value.critical
      is_critical   = api_step.value.critical ? null : false

      request_definition {
        method           = api_step.value.method
        url              = "${local.frontend_url}${api_step.value.path}"
        body             = api_step.value.form
        body_type        = api_step.value.form == null ? null : "application/x-www-form-urlencoded"
        follow_redirects = false
        timeout          = 10
      }

      request_headers = merge(
        { Cookie = local.session_cookie },
        api_step.value.form == null ? {} : { "Content-Type" = "application/x-www-form-urlencoded" },
      )

      dynamic "assertion" {
        for_each = api_step.value.assertions

        content {
          type     = assertion.value.type
          operator = assertion.value.operator
          property = assertion.value.property
          target   = assertion.value.target
        }
      }
    }
  }

  options_list {
    tick_every           = var.synthetics_tick_every
    min_failure_duration = var.synthetics_min_failure_duration
    min_location_failed  = 1
    monitor_name         = local.synthetics_monitor_name["add-to-cart"]
    monitor_priority     = 3

    retry {
      count    = var.synthetics_retry_count
      interval = 2000
    }
  }
}

resource "datadog_synthetics_test" "cuj_view_cart" {
  name      = "mamori CUJ view-cart"
  type      = "api"
  subtype   = "multi"
  status    = var.synthetics_status
  locations = var.synthetics_locations
  tags      = concat(local.datadog_tags, ["cuj:view-cart"])
  message   = local.synthetics_message["view-cart"]

  config_variable {
    type    = "text"
    name    = "SESSION_ID"
    pattern = "{{ uuid }}"
  }

  dynamic "api_step" {
    for_each = local.cuj_steps["view-cart"]

    content {
      name          = api_step.value.name
      subtype       = "http"
      allow_failure = !api_step.value.critical
      is_critical   = api_step.value.critical ? null : false

      request_definition {
        method           = api_step.value.method
        url              = "${local.frontend_url}${api_step.value.path}"
        body             = api_step.value.form
        body_type        = api_step.value.form == null ? null : "application/x-www-form-urlencoded"
        follow_redirects = false
        timeout          = 10
      }

      request_headers = merge(
        { Cookie = local.session_cookie },
        api_step.value.form == null ? {} : { "Content-Type" = "application/x-www-form-urlencoded" },
      )

      dynamic "assertion" {
        for_each = api_step.value.assertions

        content {
          type     = assertion.value.type
          operator = assertion.value.operator
          property = assertion.value.property
          target   = assertion.value.target
        }
      }
    }
  }

  options_list {
    tick_every           = var.synthetics_tick_every
    min_failure_duration = var.synthetics_min_failure_duration
    min_location_failed  = 1
    monitor_name         = local.synthetics_monitor_name["view-cart"]
    monitor_priority     = 3

    retry {
      count    = var.synthetics_retry_count
      interval = 2000
    }
  }
}

resource "datadog_synthetics_test" "cuj_checkout" {
  name      = "mamori CUJ checkout"
  type      = "api"
  subtype   = "multi"
  status    = var.synthetics_status
  locations = var.synthetics_locations
  tags      = concat(local.datadog_tags, ["cuj:checkout"])
  message   = local.synthetics_message["checkout"]

  config_variable {
    type    = "text"
    name    = "SESSION_ID"
    pattern = "{{ uuid }}"
  }

  dynamic "api_step" {
    for_each = local.cuj_steps["checkout"]

    content {
      name          = api_step.value.name
      subtype       = "http"
      allow_failure = !api_step.value.critical
      is_critical   = api_step.value.critical ? null : false

      request_definition {
        method           = api_step.value.method
        url              = "${local.frontend_url}${api_step.value.path}"
        body             = api_step.value.form
        body_type        = api_step.value.form == null ? null : "application/x-www-form-urlencoded"
        follow_redirects = false
        timeout          = 10
      }

      request_headers = merge(
        { Cookie = local.session_cookie },
        api_step.value.form == null ? {} : { "Content-Type" = "application/x-www-form-urlencoded" },
      )

      dynamic "assertion" {
        for_each = api_step.value.assertions

        content {
          type     = assertion.value.type
          operator = assertion.value.operator
          property = assertion.value.property
          target   = assertion.value.target
        }
      }
    }
  }

  options_list {
    tick_every           = var.synthetics_tick_every
    min_failure_duration = var.synthetics_min_failure_duration
    min_location_failed  = 1
    monitor_name         = local.synthetics_monitor_name["checkout"]
    monitor_priority     = 3

    retry {
      count    = var.synthetics_retry_count
      interval = 2000
    }
  }
}
