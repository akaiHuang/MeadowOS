/* Microphone-reactive layered cloud shader. No audio access, recording or dependencies.
   Restores the original orb renderer with irregular local flow, not particle sediment. */
(() => {
  'use strict';

  const INSTANCES = new WeakMap();
  const PALETTES = {
    meadow: {label:'苔綠', swatch:'#467b54', colors:[[.92,.96,.88],[.65,.79,.57],[.25,.49,.33],[.055,.25,.18]]},
    ocean: {label:'海藍', swatch:'#367fb1', colors:[[.90,.96,.99],[.53,.78,.88],[.18,.43,.65],[.035,.16,.32]]},
    violet: {label:'紫霧', swatch:'#8871ba', colors:[[.96,.93,.99],[.77,.65,.88],[.47,.32,.65],[.20,.12,.34]]},
    amber: {label:'暖金', swatch:'#bb8841', colors:[[.99,.97,.89],[.92,.77,.48],[.65,.43,.18],[.32,.20,.07]]},
    rose: {label:'莓粉', swatch:'#b66b86', colors:[[.99,.93,.94],[.89,.65,.71],[.66,.32,.45],[.31,.10,.22]]},
    mono: {label:'黑白', swatch:'#666967', colors:[[.97,.97,.96],[.75,.76,.75],[.38,.40,.39],[.10,.12,.11]]},
  };
  const VERTEX = `
    attribute vec2 a_position;
    varying vec2 v_uv;
    void main() {
      v_uv = a_position * 0.5 + 0.5;
      gl_Position = vec4(a_position, 0.0, 1.0);
    }
  `;
  const FRAGMENT = `
    precision mediump float;
    varying vec2 v_uv;
    uniform vec2 u_resolution;
    uniform float u_time;
    uniform float u_energy;
    uniform vec4 u_flow;
    uniform vec3 u_air, u_mist, u_green, u_forest;
    float hash(vec2 p) {
      vec3 q = fract(vec3(p.xyx) * 0.1031);
      q += dot(q, q.yzx + 33.33);
      return fract((q.x + q.y) * q.z);
    }
    float noise(vec2 p) {
      vec2 i=floor(p), f=fract(p), u=f*f*(3.0-2.0*f);
      return mix(mix(hash(i),hash(i+vec2(1,0)),u.x),mix(hash(i+vec2(0,1)),hash(i+vec2(1,1)),u.x),u.y);
    }
    float fbm(vec2 p) {
      float n=0.0, a=0.52;
      mat2 turn=mat2(.80,-.60,.60,.80);
      for(int i=0;i<5;i++) { n+=a*noise(p); p=turn*p*2.07+vec2(7.3,2.9); a*=.49; }
      return n;
    }
    float cloud(vec2 p, float seed, float depth) {
      vec2 drift=u_flow.xy*(.6+depth*.3);
      // Material coordinates remain continuous; the visible disk is the boundary.
      // Never mirror samples at the rim: that folds the entire cloud back inward.
      vec2 transport=p+drift;
      vec2 q=transport*1.65+vec2(seed,seed*.73);
      vec2 curl=vec2(fbm(q*.7+vec2(u_time*.055,0)),fbm(q*.7+vec2(4.7,-u_time*.047)))-.48;
      float body=fbm(q+curl*(2.0+u_energy*.85));
      float filament=fbm(q*3.2+curl*2.8+vec2(0,u_time*.025));
      // A broad billow with fine wisps, rather than blurred particle splats.
      float field=.38-p.y*.31+(body-.49)*1.95+(filament-.48)*.23;
      return smoothstep(.02,.65,field);
    }
    void main() {
      vec2 disk=v_uv*2.0-1.0;
      float radius=length(disk);
      float alpha=1.0-smoothstep(.985-2.0/u_resolution.x,.985,radius);
      if(alpha<=0.0){gl_FragColor=vec4(0);return;}
      float boundaryFlex=1.0-smoothstep(.48,.985,radius);
      vec2 p=disk;
      vec2 local=p-vec2(-.36,.12);
      p+=vec2(-local.y,local.x)*u_flow.z*boundaryFlex*exp(-dot(local,local)*2.2);
      local=p-vec2(.43,-.22);
      p+=vec2(local.y,-local.x)*u_flow.w*boundaryFlex*exp(-dot(local,local)*2.6);
      float back=cloud(p+vec2(.12,.22),4.8,.4);
      float front=cloud(p+vec2(-.08,-.12),1.3,1.0);
      float lace=fbm(p*11.0+u_flow.xy*.7+vec2(u_time*.035,-u_time*.02));
      vec3 air=u_air, mist=u_mist, green=u_green, forest=u_forest;
      vec3 color=mix(air,mist,back*.70);
      color=mix(color,green,front*.78);
      float depth=front*back;
      color=mix(color,forest,smoothstep(.2,.85,depth)*.78);
      // Light catches the thinner cloud rim; noise stays attached to the flow.
      float rim=front*(1.0-front)*(1.0-back);
      color+=vec3(.18,.21,.14)*rim*(.65+lace*.7);
      color+=(lace-.5)*.035*(front*(1.0-front)+back*(1.0-back));
      color*=1.0-.065*smoothstep(.6,1.0,radius);
      gl_FragColor=vec4(color*alpha,alpha);
    }
  `;

  const STYLES = {
    cloud: {label:'雲霧',palettes:PALETTES,fragment:FRAGMENT},
    ...(window.MeadowInkMaterial ? {glaze:{label:'結晶釉',...window.MeadowInkMaterial}} : {}),
  };

  class InkOrb {
    constructor(canvas) {
      this.canvas = canvas;
      this.host = canvas.parentElement;
      this.gl = null;
      this.program = null;
      this.buffer = null;
      this.style = Object.hasOwn(STYLES,this.host.dataset.style) ? this.host.dataset.style : 'cloud';
      this.palette = 'meadow';
      this.programs = new Map();
      this.frameId = 0;
      this.phase = 6.8;
      this.windX = 0; this.windY = 0;
      this.velocityX = 0; this.velocityY = 0;
      this.goalX = .3; this.goalY = -.2;
      this.eddyA = .2; this.eddyB = -.25;
      this.goalA = .2; this.goalB = -.25;
      this.changeIn = 0; this.flowChanges = 0;
      this.disposed = false;
      this.energy = 0;
      this.target = 0;
      this.active = false;
      this.muted = false;
      this.visible = true;
      this.suspended = false;
      this.lastInputAt = 0;
      this.lastFrameAt = 0;
      this.frames = 0;
      this.motion = window.matchMedia('(prefers-reduced-motion: reduce)');
      this.abort = new AbortController();
      const options = { signal: this.abort.signal };
      this.tick = this.tick.bind(this);
      this.init();
      this.resizeObserver = new ResizeObserver(() => this.resize());
      this.resizeObserver.observe(this.host);
      this.intersectionObserver = new IntersectionObserver(([entry]) => {
        this.visible = entry.isIntersecting;
        this.syncAnimation();
      });
      this.intersectionObserver.observe(this.host);
      this.motion.addEventListener('change', () => this.syncAnimation(), options);
      document.addEventListener('visibilitychange', () => this.syncAnimation(), options);
      window.addEventListener('resize', () => this.resize(), options);
      window.addEventListener('pagehide', () => { this.suspended = true; this.syncAnimation(); }, options);
      window.addEventListener('pageshow', () => { this.suspended = false; this.syncAnimation(); }, options);
      canvas.addEventListener('webglcontextlost', (event) => {
        event.preventDefault();
        this.stop();
        this.host.dataset.renderer = 'fallback';
      }, options);
      canvas.addEventListener('webglcontextrestored', () => { this.init(); this.syncAnimation(); }, options);
      this.syncAnimation();
    }

    init() {
      try {
        const gl = this.canvas.getContext('webgl', {
          alpha: true, antialias: false, depth: false, stencil: false,
          premultipliedAlpha: true, powerPreference: 'low-power',
        });
        if (!gl || gl.isContextLost()) throw new Error('WebGL unavailable');
        this.gl = gl;
        const compile = (type, source) => {
          const shader = gl.createShader(type);
          gl.shaderSource(shader, source);
          gl.compileShader(shader);
          if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
            gl.deleteShader(shader);
            throw new Error('Ink shader unavailable');
          }
          return shader;
        };
        this.compileShader = compile;
        this.programs = new Map();
        this.useStyle(this.style);
        this.buffer = gl.createBuffer();
        gl.bindBuffer(gl.ARRAY_BUFFER, this.buffer);
        gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 1, -1, -1, 1, -1, 1, 1, -1, 1, 1]), gl.STATIC_DRAW);
        gl.useProgram(this.program);
        const position = gl.getAttribLocation(this.program, 'a_position');
        gl.enableVertexAttribArray(position);
        gl.vertexAttribPointer(position, 2, gl.FLOAT, false, 0, 0);
        this.host.dataset.renderer = 'loading';
        this.resize();
      } catch (_) {
        this.host.dataset.renderer = 'fallback';
        this.stop();
      }
    }

    resize() {
      if (!this.gl || this.gl.isContextLost() || !this.program) return;
      // A single small shader surface, capped independently of phone DPR.
      const size = Math.max(1, Math.min(384, Math.round(this.host.clientWidth * Math.min(window.devicePixelRatio || 1, 2))));
      if (this.canvas.width !== size || this.canvas.height !== size) {
        this.canvas.width = size;
        this.canvas.height = size;
        this.gl.viewport(0, 0, size, size);
      }
      this.draw();
    }

    setInput(rms) {
      if (this.disposed) return;
      const value = Number.isFinite(rms) ? Math.max(0, rms) : 0;
      this.target = this.active && !this.muted ? Math.min(1, Math.pow(Math.max(0, value - 0.003) / 0.035, 0.60)) : 0;
      this.lastInputAt = performance.now();
    }

    setState(active, muted) {
      if (this.disposed) return;
      this.active = Boolean(active);
      this.muted = Boolean(muted);
      if (!this.active || this.muted) this.target = 0;
      this.syncAnimation();
    }

    useStyle(style) {
      const gl=this.gl;
      if(!this.programs.has(style)) {
        const vertex=this.compileShader(gl.VERTEX_SHADER,VERTEX);
        const high=gl.getShaderPrecisionFormat(gl.FRAGMENT_SHADER,gl.HIGH_FLOAT)?.precision;
        const source=STYLES[style].fragment;
        let fragment,program;
        try {
          fragment=this.compileShader(gl.FRAGMENT_SHADER,high ? source.replace('precision mediump','precision highp') : source);
          program=gl.createProgram();gl.attachShader(program,vertex);gl.attachShader(program,fragment);gl.linkProgram(program);
          if(!gl.getProgramParameter(program,gl.LINK_STATUS)) throw new Error('Material unavailable');
          const uniforms=Object.fromEntries(['resolution','time','energy','flow','air','mist','green','forest'].map(key=>[key,gl.getUniformLocation(program,'u_'+key)]));
          this.programs.set(style,{program,uniforms});
        } catch(error) {
          if(program) gl.deleteProgram(program);
          throw error;
        } finally {gl.deleteShader(vertex);if(fragment) gl.deleteShader(fragment);}
      }
      const cached=this.programs.get(style);
      this.program=cached.program;this.uniforms=cached.uniforms;
      this.style=style;this.host.dataset.style=style;
    }

    setAppearance({style = this.style, palette = this.palette} = {}) {
      if(this.disposed) return false;
      if(!Object.hasOwn(STYLES,style)) style=this.style;
      if(style!==this.style) {
        if(this.gl && !this.gl.isContextLost() && this.program) {
          try {this.useStyle(style);} catch (_) {return false;}
        } else {this.style=style;this.host.dataset.style=style;}
      }
      this.palette=Object.hasOwn(STYLES[this.style].palettes,palette) ? palette : 'meadow';
      this.draw();
      return true;
    }

    bindProgram(program) {
      const gl=this.gl;
      gl.useProgram(program); gl.bindBuffer(gl.ARRAY_BUFFER,this.buffer);
      const position=gl.getAttribLocation(program,'a_position');
      gl.enableVertexAttribArray(position); gl.vertexAttribPointer(position,2,gl.FLOAT,false,0,0);
    }

    draw() {
      const gl = this.gl;
      if (!gl || gl.isContextLost() || !this.program || this.disposed) return;
      gl.bindFramebuffer(gl.FRAMEBUFFER,null);
      gl.viewport(0,0,this.canvas.width,this.canvas.height);
      this.bindProgram(this.program);
      gl.uniform2f(this.uniforms.resolution, this.canvas.width, this.canvas.height);
      gl.uniform1f(this.uniforms.time, this.phase);
      gl.uniform1f(this.uniforms.energy, this.motion.matches ? 0 : this.energy);
      gl.uniform4f(this.uniforms.flow, this.windX, this.windY, this.eddyA, this.eddyB);
      const colors=STYLES[this.style].palettes[this.palette].colors;
      ['air','mist','green','forest'].forEach((key,index)=>gl.uniform3fv(this.uniforms[key],colors[index]));
      gl.drawArrays(gl.TRIANGLES, 0, 6);
      this.frames++;
      // The static cloud remains visible until an actual cloud frame is drawn.
      this.host.dataset.renderer='webgl';
    }

    advanceFlow(dt, speed) {
      // This is a continuous material offset, not a particle inside the disk.
      // The circular mask and tapered local eddies define the visible boundary.
      this.windX += this.velocityX * dt * speed;
      this.windY += this.velocityY * dt * speed;
    }

    tick(now) {
      this.frameId = 0;
      if (!this.canAnimate()) return;
      const interval = 1000 / (this.active || this.energy > .025 ? 30 : 24);
      const elapsed = now - this.lastFrameAt;
      if (elapsed >= interval - 1) {
        const dt = Math.min(0.1, elapsed / 1000);
        this.lastFrameAt = now;
        const target = now - this.lastInputAt > 300 ? 0 : this.target;
        // Fast acoustic response, long inertial tail. Muting / ending only
        // removes the driving force; it never zeroes the cloud's movement.
        const smoothing = 1 - Math.exp(-dt / (target > this.energy ? 0.045 : 3.4));
        this.energy += (target - this.energy) * smoothing;
        this.changeIn -= dt;
        if (this.changeIn <= 0) {
          const angle = Math.random() * Math.PI * 2;
          this.goalX = Math.cos(angle); this.goalY = Math.sin(angle);
          this.goalA = (Math.random() * 2 - 1) * 1.6;
          this.goalB = (Math.random() * 2 - 1) * 1.6;
          this.changeIn = .8 + Math.random() * 1.5 + (1 - this.energy) * 2;
          this.flowChanges++;
        }
        const turn = 1 - Math.exp(-dt / (1.4 - this.energy * .75));
        this.velocityX += (this.goalX - this.velocityX) * turn;
        this.velocityY += (this.goalY - this.velocityY) * turn;
        // The breeze never reaches zero, including before the first sound.
        const speed = .075 + this.energy * 1.45;
        this.advanceFlow(dt, speed);
        const strength = .27 + this.energy * 1.1;
        this.eddyA += (this.goalA * strength - this.eddyA) * turn;
        this.eddyB += (this.goalB * strength - this.eddyB) * turn;
        this.phase += dt * (.26 + this.energy * 2.6);
        this.draw();
      }
      this.frameId = requestAnimationFrame(this.tick);
    }

    canAnimate() {
      return !this.disposed && this.host.dataset.renderer === 'webgl' && !this.gl.isContextLost() && !this.motion.matches && !document.hidden && this.visible && !this.suspended;
    }

    syncAnimation() {
      if (!this.canAnimate()) { this.stop(); return; }
      if (!this.frameId) {
        this.lastFrameAt = performance.now();
        this.frameId = requestAnimationFrame(this.tick);
      }
    }

    stop() {
      cancelAnimationFrame(this.frameId);
      this.frameId = 0;
      this.lastFrameAt = 0;
    }

    dispose() {
      this.disposed = true;
      this.stop();
      this.abort.abort();
      this.resizeObserver.disconnect();
      this.intersectionObserver.disconnect();
      if (this.gl && !this.gl.isContextLost()) {
        this.gl.deleteBuffer(this.buffer);
        for(const {program} of this.programs.values()) this.gl.deleteProgram(program);
        this.programs.clear();
      }
    }
  }

  window.MeadowInkOrb = {
    palettes: PALETTES,
    styles: STYLES,
    get: canvas => INSTANCES.get(canvas),
    create(canvas) {
      if (!canvas) return null;
      try { const orb=new InkOrb(canvas); INSTANCES.set(canvas,orb); return orb; } catch (_) { canvas.parentElement.dataset.renderer = 'fallback'; return null; }
    },
  };
})();
