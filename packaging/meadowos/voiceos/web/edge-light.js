/* Diffuse light behind the screen edge. Low-frequency advection, no rings or
 * visible border. A small independent surface keeps full-screen fill inexpensive. */
(() => {
  'use strict';

  const VERTEX = `
    attribute vec2 a_position;
    varying vec2 v_uv;
    void main() { v_uv = a_position * .5 + .5; gl_Position = vec4(a_position, 0., 1.); }
  `;
  const FRAGMENT = `
    #ifdef GL_FRAGMENT_PRECISION_HIGH
      precision highp float;
    #else
      precision mediump float;
    #endif
    varying vec2 v_uv;
    uniform float u_time, u_energy;
    uniform vec3 u_light, u_mist;
    float hash(vec2 p) {
      #ifdef GL_FRAGMENT_PRECISION_HIGH
        return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453);
      #else
        // On true float16 mediump, values near 43758 have no fractional bits.
        // Keep every intermediate below eight so the fallback retains noise.
        vec2 q = fract(p*vec2(.1031,.11369));
        float a = fract((q.x+q.y)*2.73);
        float b = fract((q.x*q.y+.17)*5.31);
        return fract((a+b)*3.71);
      #endif
    }
    float noise(vec2 p) {
      vec2 i = floor(p), f = fract(p); f = f*f*(3.-2.*f);
      return mix(mix(hash(i),hash(i+vec2(1.,0.)),f.x),mix(hash(i+vec2(0.,1.)),hash(i+vec2(1.)),f.x),f.y);
    }
    float field(vec2 p) { return noise(p)*.65 + noise(p*2.03+8.1)*.25 + noise(p*4.07+1.7)*.10; }
    float bleed(float distance, float along, float seed, float time, float breadth) {
      // Independent elongated sources run along each physical screen edge.
      // Only width and brightness drift; there are no traveling wave fronts.
      // Broad lobes change the depth by several times, while finer variations
      // soften their shoulders. Each side uses unrelated coordinates and pace.
      float broad = noise(vec2(along*4.6-time*.62,seed+11.7));
      float shoulder = noise(vec2(along*9.3+time*.31,seed+3.2));
      float lobe = smoothstep(.20,.80,broad*.82+shoulder*.18);
      float width = (.035 + .170*pow(lobe,1.18))*breadth;
      float glow = field(vec2(along*5.7+time,seed));
      float gap = smoothstep(.15,.69,noise(vec2(along*7.1-time*.43,seed+37.4)));
      float strength = (.26 + .64*glow)*(.46 + .54*gap);
      return exp(-pow(max(0.,distance)/width,1.55)*1.9)*strength;
    }
    void main() {
      vec2 p = vec2(v_uv.x, 1.-v_uv.y);
      float t = u_time*.045;
      float top = bleed(p.y,p.x,1.3,t,.88)*.86;
      float bottom = bleed(1.-p.y,p.x,8.7,-t*.81,1.16)*.93;
      float left = bleed(p.x,p.y,15.4,-t*.67,.76)*.83;
      float right = bleed(1.-p.x,p.y,22.1,t*.73,1.04)*.90;
      // Screen blending avoids bright corner pools when two bands meet.
      float intensity = (1.-(1.-top)*(1.-bottom)*(1.-left)*(1.-right))*(1.+u_energy*.08);
      vec3 color = mix(u_light,u_mist,.18)*.57;
      // Light is strongest just inside all four edges, fading into a dark center.
      vec3 base = vec3(.0353,.0431,.0471);
      gl_FragColor = vec4(base+color*intensity,1.);
    }
  `;

  class EdgeLight {
    constructor(canvas) {
      this.canvas = canvas;
      this.gl = null;
      this.program = null;
      this.buffer = null;
      this.uniforms = null;
      this.frame = 0;
      this.time = 6.8;
      this.lastFrame = 0;
      this.energy = 0;
      this.light = [.19, .43, .52];
      this.mist = [.53, .75, .68];
      this.compact = false;
      this.disposed = false;
      this.suspended = false;
      this.motion = matchMedia('(prefers-reduced-motion: reduce)');
      this.abort = new AbortController();
      const options = {signal: this.abort.signal};
      this.tick = this.tick.bind(this);
      this.init();
      this.resizeObserver = new ResizeObserver(() => this.resize());
      this.resizeObserver.observe(canvas);
      this.motion.addEventListener('change', () => this.sync(), options);
      document.addEventListener('visibilitychange', () => this.sync(), options);
      window.addEventListener('pagehide', () => { this.suspended = true; this.sync(); }, options);
      window.addEventListener('pageshow', () => { this.suspended = false; this.sync(); }, options);
      canvas.addEventListener('webglcontextlost', event => {
        event.preventDefault();
        this.stop();
        document.body.dataset.edgeRenderer = 'fallback';
      }, options);
      canvas.addEventListener('webglcontextrestored', () => { this.init(); this.sync(); }, options);
      this.sync();
    }

    init() {
      try {
        const gl = this.canvas.getContext('webgl', {
          alpha: false, antialias: false, depth: false, stencil: false,
          preserveDrawingBuffer: false, powerPreference: 'low-power',
        });
        if (!gl || gl.isContextLost()) throw new Error('No WebGL');
        this.gl = gl;
        const precision = gl.getShaderPrecisionFormat(gl.FRAGMENT_SHADER, gl.HIGH_FLOAT);
        document.body.dataset.edgePrecision = precision && precision.precision > 0 ? 'highp' : 'mediump';
        const compile = (type, source) => {
          const shader = gl.createShader(type);
          gl.shaderSource(shader, source); gl.compileShader(shader);
          if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
            gl.deleteShader(shader); throw new Error('Shader unavailable');
          }
          return shader;
        };
        const vertex = compile(gl.VERTEX_SHADER, VERTEX);
        let fragment;
        let program;
        try {
          fragment = compile(gl.FRAGMENT_SHADER, FRAGMENT);
          program = gl.createProgram();
          gl.attachShader(program, vertex); gl.attachShader(program, fragment); gl.linkProgram(program);
          if (!gl.getProgramParameter(program, gl.LINK_STATUS)) throw new Error('Program unavailable');
        } catch (error) {
          if (program) gl.deleteProgram(program);
          throw error;
        } finally {
          gl.deleteShader(vertex);
          if (fragment) gl.deleteShader(fragment);
        }
        this.program = program;
        this.uniforms = Object.fromEntries(['time', 'energy', 'light', 'mist'].map(key => [key, gl.getUniformLocation(program, 'u_' + key)]));
        this.buffer = gl.createBuffer();
        gl.bindBuffer(gl.ARRAY_BUFFER, this.buffer);
        gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1,-1,1,-1,-1,1,-1,1,1,-1,1,1]), gl.STATIC_DRAW);
        this.resize();
      } catch (_) {
        document.body.dataset.edgeRenderer = 'fallback';
        this.stop();
      }
    }

    resize() {
      if (!this.gl || this.gl.isContextLost() || !this.program || this.compact) return;
      // Edge fields have no fine detail. Cap raster area even on a 4K display.
      const aspect = Math.max(.25, this.canvas.clientHeight / Math.max(1, this.canvas.clientWidth));
      const width = Math.max(160, Math.min(640, Math.round(this.canvas.clientWidth * .4)));
      const height = Math.max(160, Math.min(640, Math.round(width * aspect)));
      if (this.canvas.width !== width || this.canvas.height !== height) {
        this.canvas.width = width; this.canvas.height = height;
      }
      this.draw();
    }

    setState({colors, energy, compact} = {}) {
      if (colors) { this.light = colors[2]; this.mist = colors[1]; }
      if (Number.isFinite(energy)) this.energy = Math.max(0, Math.min(1, energy));
      const resized = typeof compact === 'boolean' && compact !== this.compact;
      if (typeof compact === 'boolean') this.compact = compact;
      if (resized) this.resize();
      else if (colors) this.draw();
      this.sync();
    }

    draw() {
      const gl = this.gl;
      if (this.disposed || this.compact || !gl || gl.isContextLost() || !this.program) return;
      gl.viewport(0, 0, this.canvas.width, this.canvas.height);
      gl.useProgram(this.program); gl.bindBuffer(gl.ARRAY_BUFFER, this.buffer);
      const position = gl.getAttribLocation(this.program, 'a_position');
      gl.enableVertexAttribArray(position); gl.vertexAttribPointer(position, 2, gl.FLOAT, false, 0, 0);
      gl.uniform1f(this.uniforms.time, this.time);
      gl.uniform1f(this.uniforms.energy, this.motion.matches ? 0 : this.energy);
      gl.uniform3fv(this.uniforms.light, this.light); gl.uniform3fv(this.uniforms.mist, this.mist);
      gl.drawArrays(gl.TRIANGLES, 0, 6);
      document.body.dataset.edgeRenderer = 'webgl';
    }

    canAnimate() {
      return !this.disposed && !this.compact && !this.suspended && !document.hidden && !this.motion.matches && this.gl && !this.gl.isContextLost() && document.body.dataset.edgeRenderer === 'webgl';
    }

    tick(now) {
      this.frame = 0;
      if (!this.canAnimate()) return;
      const elapsed = now - this.lastFrame;
      // A slowly moving backlight does not need the orb's faster acoustic cadence.
      if (elapsed >= 1000 / 18 - 1) {
        this.time += Math.min(.1, elapsed / 1000);
        this.lastFrame = now; this.draw();
      }
      this.frame = requestAnimationFrame(this.tick);
    }

    sync() {
      if (!this.canAnimate()) { this.stop(); return; }
      if (!this.frame) { this.lastFrame = performance.now(); this.frame = requestAnimationFrame(this.tick); }
    }

    stop() { cancelAnimationFrame(this.frame); this.frame = 0; this.lastFrame = 0; }

    dispose() {
      this.disposed = true; this.stop(); this.abort.abort(); this.resizeObserver.disconnect();
      if (this.gl && !this.gl.isContextLost()) { this.gl.deleteBuffer(this.buffer); this.gl.deleteProgram(this.program); }
    }
  }

  window.MeadowEdgeLight = EdgeLight;
})();
