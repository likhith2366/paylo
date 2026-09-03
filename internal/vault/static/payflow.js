/**
 * payflow.js — the embeddable checkout widget (§2.2, §2.4).
 *
 * Deliberately vanilla JavaScript with zero dependencies, distributed as a
 * plain <script> tag. Merchants build their sites in React, Vue, Angular,
 * WordPress, or hand-written HTML; a widget that required any of those would
 * force a dependency on every merchant's stack. This is why the real Stripe.js
 * is also framework-agnostic — necessity, not preference.
 *
 * All this script does is create an iframe pointing at PayFlow's own origin and
 * relay messages to and from it. It never touches card data: the customer types
 * into a document this script cannot read, because the browser's same-origin
 * policy forbids it. That is the entire PCI-scope argument, and it only holds
 * as long as this file stays on the outside of that boundary.
 *
 * Usage:
 *
 *   <script src="https://js.payflow.com/v1/payflow.js"></script>
 *   <script>
 *     const payflow = PayFlow('pk_test_...', { vaultUrl: 'https://vault.payflow.com' });
 *     const card = payflow.createCardElement('#card-mount');
 *
 *     card.on('ready', () => console.log('mounted'));
 *     card.on('change', ({ network }) => updateIcon(network));
 *
 *     document.querySelector('#pay').addEventListener('click', async () => {
 *       const { token, error } = await card.tokenize();
 *       if (error) return showError(error.message);
 *       // Send the token to YOUR server, which calls POST /v1/charges with it.
 *       await fetch('/checkout', {
 *         method: 'POST',
 *         headers: { 'Content-Type': 'application/json' },
 *         body: JSON.stringify({ payment_token: token }),
 *       });
 *     });
 *   </script>
 */
(function (global) {
  'use strict';

  var DEFAULT_VAULT_URL = 'http://localhost:8081';
  var EVENTS = ['ready', 'change', 'error', 'token', 'resize'];

  function PayFlow(publishableKey, options) {
    if (!(this instanceof PayFlow)) return new PayFlow(publishableKey, options);

    if (!publishableKey) {
      throw new Error('PayFlow: a publishable key is required');
    }
    // A secret key in browser JavaScript is a serious mistake — it would let
    // anyone who views source charge cards. Fail loudly rather than working.
    if (publishableKey.indexOf('sk_') === 0) {
      throw new Error(
        'PayFlow: that is a SECRET key. Never put a secret key in client-side ' +
        'code — use your publishable key (pk_...) here.'
      );
    }

    options = options || {};
    this.publishableKey = publishableKey;
    this.vaultUrl = (options.vaultUrl || DEFAULT_VAULT_URL).replace(/\/$/, '');
    this.merchantId = options.merchantId || '';
  }

  PayFlow.prototype.createCardElement = function (target, options) {
    return new CardElement(this, target, options || {});
  };

  function CardElement(client, target, options) {
    var mount = typeof target === 'string' ? document.querySelector(target) : target;
    if (!mount) {
      throw new Error('PayFlow: mount target "' + target + '" was not found');
    }

    this.client = client;
    this.mount = mount;
    this.handlers = {};
    this.ready = false;
    this.pending = null;

    EVENTS.forEach(function (name) { this.handlers[name] = []; }, this);

    this._build(options);
    this._listen();
  }

  CardElement.prototype._build = function (options) {
    var url =
      this.client.vaultUrl + '/checkout/iframe.html' +
      '?origin=' + encodeURIComponent(window.location.origin) +
      '&vault=' + encodeURIComponent(this.client.vaultUrl) +
      (this.client.merchantId ? '&merchant_id=' + encodeURIComponent(this.client.merchantId) : '');

    var frame = document.createElement('iframe');
    frame.src = url;
    frame.title = 'Secure card input';
    frame.setAttribute('allowtransparency', 'true');
    // allow-forms is deliberately absent: the frame tokenizes over fetch and
    // never performs a native form submission, so granting it would widen the
    // sandbox for no reason.
    frame.setAttribute('sandbox', 'allow-scripts allow-same-origin');
    frame.style.cssText =
      'width:100%;height:' + (options.height || 168) + 'px;border:0;' +
      'display:block;overflow:hidden;color-scheme:normal;';

    this.frame = frame;
    this.mount.appendChild(frame);
  };

  CardElement.prototype._listen = function () {
    var self = this;

    this._onMessage = function (event) {
      // Two checks, both required. The origin check rejects messages from any
      // other frame on the page; the source tag rejects unrelated messages
      // from our own origin (extensions and dev tools post plenty).
      if (event.origin !== self.client.vaultUrl) return;
      if (!event.data || event.data.source !== 'payflow-checkout') return;

      var data = event.data;

      switch (data.type) {
        case 'ready':
          self.ready = true;
          break;

        case 'resize':
          if (data.height) self.frame.style.height = data.height + 'px';
          break;

        case 'token':
          if (self.pending) {
            self.pending.resolve({
              token: data.token,
              brand: data.brand,
              last4: data.last4,
              expMonth: data.expMonth,
              expYear: data.expYear,
              error: null,
            });
            self.pending = null;
          }
          break;

        case 'error':
          if (self.pending) {
            // Resolve rather than reject: a declined or mistyped card is an
            // expected outcome, not an exception. Merchants shouldn't need a
            // try/catch around an invalid CVC.
            self.pending.resolve({
              token: null,
              error: { code: data.code, message: data.message },
            });
            self.pending = null;
          }
          break;
      }

      self._emit(data.type, data);
    };

    window.addEventListener('message', this._onMessage);
  };

  CardElement.prototype._post = function (type, payload) {
    var message = { source: 'payflow-parent', type: type };
    for (var key in payload) {
      if (Object.prototype.hasOwnProperty.call(payload, key)) message[key] = payload[key];
    }
    // Targeted at the vault's exact origin, never '*'.
    this.frame.contentWindow.postMessage(message, this.client.vaultUrl);
  };

  CardElement.prototype._emit = function (name, data) {
    (this.handlers[name] || []).forEach(function (fn) {
      try {
        fn(data);
      } catch (err) {
        // A throwing merchant callback must not break the widget for the
        // customer, who would otherwise be stuck at a dead checkout.
        if (global.console && console.error) console.error('PayFlow: handler for "' + name + '" threw', err);
      }
    });
  };

  CardElement.prototype.on = function (name, handler) {
    if (!this.handlers[name]) this.handlers[name] = [];
    this.handlers[name].push(handler);
    return this;
  };

  /**
   * Exchange the entered card for a single-use token.
   * Resolves with { token, brand, last4, error } — errors are values, not throws.
   */
  CardElement.prototype.tokenize = function () {
    var self = this;

    if (this.pending) {
      return Promise.resolve({
        token: null,
        error: { code: 'tokenize_in_progress', message: 'A tokenization is already in progress.' },
      });
    }

    return new Promise(function (resolve) {
      var settled = false;
      var timer = setTimeout(function () {
        if (settled) return;
        settled = true;
        self.pending = null;
        resolve({
          token: null,
          error: { code: 'timeout', message: 'The request timed out. Please try again.' },
        });
      }, 20000);

      self.pending = {
        resolve: function (result) {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          resolve(result);
        },
      };

      self._post('tokenize');
    });
  };

  CardElement.prototype.focus = function () { this._post('focus'); return this; };
  CardElement.prototype.clear = function () { this._post('clear'); return this; };

  CardElement.prototype.destroy = function () {
    window.removeEventListener('message', this._onMessage);
    if (this.frame && this.frame.parentNode) this.frame.parentNode.removeChild(this.frame);
    this.pending = null;
  };

  PayFlow.version = '1.0.0';
  global.PayFlow = PayFlow;
})(typeof window !== 'undefined' ? window : this);
